package vault

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"os"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"
)

// Name the Secret Service store is reported by
const keychainName = "Secret Service"

// Secret Service bus names, object paths, interfaces, session parameters and the item schema libsecret reads
const (
	ssBus         = "org.freedesktop.secrets"
	ssPath        = dbus.ObjectPath("/org/freedesktop/secrets")
	ssDefault     = dbus.ObjectPath("/org/freedesktop/secrets/aliases/default")
	ssService     = "org.freedesktop.Secret.Service"
	ssCollection  = "org.freedesktop.Secret.Collection"
	ssItem        = "org.freedesktop.Secret.Item"
	ssPrompt      = "org.freedesktop.Secret.Prompt"
	ssAlgorithm   = "dh-ietf1024-sha256-aes128-cbc-pkcs7"
	ssApplication = "icebath"
	ssSchema      = "org.freedesktop.Secret.Generic"
	ssDismissWait = 2 * time.Second
	ssCallTimeout = 25 * time.Second
)

// Second Oakley group prime of RFC 2409 the encrypted transfer session agrees its key over
const ssPrimeHex = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE65381FFFFFFFFFFFFFFFF"

// Error kind for a Secret Service reply that does not have the shape the specification gives it
var errSecretServiceReply = errors.New("malformed Secret Service reply")

// Secret Service state, empty because each operation opens its own session bus connection
type keychainState struct{}

// Secret as the Secret Service transfers it, session, IV, value and content type
type ssSecret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

// One session bus connection with the transfer session it opened
type secretService struct {
	conn    *dbus.Conn
	release context.CancelFunc
	session dbus.ObjectPath
	aesKey  []byte
}

// Connection to the session bus the environment names, never one this process would start, its handshake bounded
func dialSecretService(ctx context.Context) (*secretService, error) {
	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if addr == "" {
		return nil, noKeychain("DBUS_SESSION_BUS_ADDRESS is unset")
	}
	life, release := context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, release)
	timer := time.AfterFunc(ssCallTimeout, release)
	conn, err := dbus.Connect(addr, dbus.WithContext(life))
	timed, canceled := !timer.Stop(), !stop()
	switch {
	case err == nil && !timed && !canceled:
		return &secretService{conn: conn, release: release}, nil
	case err == nil:
		_ = conn.Close()
	}
	release()
	if canceled && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err == nil {
		err = errors.New("the handshake did not finish in time")
	}
	return nil, noKeychain(fmt.Sprintf("the session bus refused a connection: %v", err))
}

// Closes the connection and with it the transfer session
func (s *secretService) close() {
	_ = s.conn.Close()
	s.release()
}

// Name of the D-Bus error behind err, empty for any other failure
func dbusErrorName(err error) string {
	var value dbus.Error
	if errors.As(err, &value) {
		return value.Name
	}
	var ptr *dbus.Error
	if errors.As(err, &ptr) {
		return ptr.Name
	}
	return ""
}

// Keychain error for a failed Secret Service call, missing objects and an absent daemon told apart
func ssError(err error) error {
	if err == nil {
		return nil
	}
	var ke *KeychainError
	if errors.As(err, &ke) {
		return err
	}
	switch dbusErrorName(err) {
	case "org.freedesktop.DBus.Error.UnknownMethod", "org.freedesktop.Secret.Error.NoSuchObject":
		return keychainError(ErrNotFound, "the Secret Service has no such object (%v)", err)
	case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.Spawn.ExecFailed", "org.freedesktop.DBus.Error.NoReply":
		return noKeychain(fmt.Sprintf("the Secret Service is not available: %v", err))
	}
	return &KeychainError{Kind: err, Reason: fmt.Sprintf("the Secret Service failed: %v", err)}
}

// Error for a reply whose shape the specification does not allow
func malformed(format string, args ...any) error {
	return keychainError(errSecretServiceReply, "the Secret Service "+format, args...)
}

// Method invocation on a Secret Service object, its reply awaited no longer than the D-Bus default timeout
func (s *secretService) call(ctx context.Context, path dbus.ObjectPath, method string, args ...any) *dbus.Call {
	ctx, cancel := context.WithTimeout(ctx, ssCallTimeout)
	defer cancel()
	return s.conn.Object(ssBus, path).CallWithContext(ctx, method, 0, args...)
}

// Method invocation on the bus daemon, its reply awaited no longer than the D-Bus default timeout
func (s *secretService) busCall(ctx context.Context, method string, args ...any) *dbus.Call {
	ctx, cancel := context.WithTimeout(ctx, ssCallTimeout)
	defer cancel()
	return s.conn.BusObject().CallWithContext(ctx, method, 0, args...)
}

// Locked property of a collection or item
func (s *secretService) locked(ctx context.Context, path dbus.ObjectPath, iface string) (bool, error) {
	var v dbus.Variant
	if err := s.call(ctx, path, "org.freedesktop.DBus.Properties.Get", iface, "Locked").Store(&v); err != nil {
		return false, ssError(err)
	}
	locked, ok := v.Value().(bool)
	if !ok {
		return false, malformed("reported a Locked property of type %s", v.Signature())
	}
	return locked, nil
}

// Shows a Secret Service dialog and waits for its Completed signal, dismissing it when the wait ends first
func (s *secretService) prompt(ctx context.Context, path dbus.ObjectPath, what string) (bool, dbus.Variant, error) {
	if unattended(ctx) {
		return false, dbus.Variant{}, keychainError(ErrDenied, "%s needs an answer no one can give in this session, so the keychain stays locked", what)
	}
	wait, cancel := promptContext(ctx)
	defer cancel()
	var owner string
	if err := s.busCall(wait, "org.freedesktop.DBus.GetNameOwner", ssBus).Store(&owner); err != nil {
		return false, dbus.Variant{}, ssError(err)
	}
	signals := make(chan *dbus.Signal, 16)
	s.conn.Signal(signals)
	defer s.conn.RemoveSignal(signals)
	match := []dbus.MatchOption{dbus.WithMatchSender(ssBus), dbus.WithMatchObjectPath(path), dbus.WithMatchInterface(ssPrompt),
		dbus.WithMatchMember("Completed")}
	if err := s.conn.AddMatchSignalContext(wait, match...); err != nil {
		return false, dbus.Variant{}, ssError(err)
	}
	defer func() { _ = s.conn.RemoveMatchSignal(match...) }()
	if err := s.call(wait, path, ssPrompt+".Prompt", "").Err; err != nil {
		return false, dbus.Variant{}, ssError(err)
	}
	for {
		select {
		case <-wait.Done():
			dismiss, stop := context.WithTimeout(context.WithoutCancel(ctx), ssDismissWait)
			_ = s.call(dismiss, path, ssPrompt+".Dismiss").Err
			stop()
			return false, dbus.Variant{}, promptUnanswered(ctx, what)
		case sig, ok := <-signals:
			if !ok {
				return false, dbus.Variant{}, noKeychain("the session bus closed while a prompt was open")
			}
			if sig.Sender != owner || sig.Path != path || sig.Name != ssPrompt+".Completed" {
				continue
			}
			if len(sig.Body) != 2 {
				return false, dbus.Variant{}, malformed("completed a prompt with %d values", len(sig.Body))
			}
			dismissed, okDismissed := sig.Body[0].(bool)
			result, okResult := sig.Body[1].(dbus.Variant)
			if !okDismissed || !okResult {
				return false, dbus.Variant{}, malformed("completed a prompt with values that are not a boolean and a variant")
			}
			return dismissed, result, nil
		}
	}
}

// Object path a completed prompt produced
func promptPath(result dbus.Variant) (dbus.ObjectPath, error) {
	path, ok := result.Value().(dbus.ObjectPath)
	if !ok || len(path) <= 1 {
		return "", malformed("completed a prompt without an object path")
	}
	return path, nil
}

// Asks the Secret Service to open objects, showing its dialog when it needs one
func (s *secretService) unlock(ctx context.Context, what string, paths ...dbus.ObjectPath) error {
	var unlocked []dbus.ObjectPath
	var prompt dbus.ObjectPath
	if err := s.call(ctx, ssPath, ssService+".Unlock", paths).Store(&unlocked, &prompt); err != nil {
		return ssError(err)
	}
	if len(prompt) > 1 {
		_, _, err := s.prompt(ctx, prompt, what)
		return err
	}
	return nil
}

// Default collection, created when the alias names none and unlocked when locked
func (s *secretService) defaultCollection(ctx context.Context) (dbus.ObjectPath, error) {
	path := ssDefault
	var label dbus.Variant
	if err := s.call(ctx, path, "org.freedesktop.DBus.Properties.Get", ssCollection, "Label").Store(&label); err != nil {
		mapped := ssError(err)
		if !errors.Is(mapped, ErrNotFound) {
			return "", initFailure(mapped)
		}
		created, err := s.createDefault(ctx)
		if err != nil {
			return "", initFailure(err)
		}
		path = created
	}
	locked, err := s.locked(ctx, path, ssCollection)
	if err != nil {
		return "", err
	}
	if locked {
		if err := s.unlock(ctx, "the Secret Service unlock prompt", path); err != nil {
			return "", err
		}
		if locked, err = s.locked(ctx, path, ssCollection); err != nil {
			return "", err
		}
		if locked {
			return "", keychainError(ErrDenied, "Failed to unlock the collection!")
		}
	}
	return path, nil
}

// Error for a default collection that could not be opened or created, an absent daemon kept as unavailable
func initFailure(err error) error {
	if errors.Is(err, ErrUnavailable) {
		return err
	}
	var kind error = err
	if errors.Is(err, ErrDenied) {
		kind = ErrDenied
	}
	return &KeychainError{Kind: kind, Reason: fmt.Sprintf("Failed to create the collection: %v.", err)}
}

// Creates the collection the default alias names
func (s *secretService) createDefault(ctx context.Context) (dbus.ObjectPath, error) {
	props := map[string]dbus.Variant{ssCollection + ".Label": dbus.MakeVariant("Default")}
	var path, prompt dbus.ObjectPath
	if err := s.call(ctx, ssPath, ssService+".CreateCollection", props, "default").Store(&path, &prompt); err != nil {
		return "", ssError(err)
	}
	if len(path) > 1 {
		return path, nil
	}
	dismissed, result, err := s.prompt(ctx, prompt, "the Secret Service prompt creating the default collection")
	if err != nil {
		return "", err
	}
	if dismissed {
		return "", keychainError(ErrDenied, "Prompt dismissed.")
	}
	return promptPath(result)
}

// Opens an encrypted transfer session, a plain one when the service does not support the algorithm
func (s *secretService) openSession(ctx context.Context) error {
	prime, _ := new(big.Int).SetString(ssPrimeHex, 16)
	private := make([]byte, 0x80)
	if _, err := rand.Read(private); err != nil {
		return err
	}
	priv := new(big.Int).SetBytes(private)
	pub := new(big.Int).Exp(big.NewInt(2), priv, prime)
	var output dbus.Variant
	var path dbus.ObjectPath
	err := s.call(ctx, ssPath, ssService+".OpenSession", ssAlgorithm, dbus.MakeVariant(pub.Bytes())).Store(&output, &path)
	if dbusErrorName(err) == "org.freedesktop.DBus.Error.NotSupported" {
		if err := s.call(ctx, ssPath, ssService+".OpenSession", "plain", dbus.MakeVariant("")).Store(&output, &path); err != nil {
			return ssError(err)
		}
		s.session = path
		return nil
	}
	if err != nil {
		return ssError(err)
	}
	raw, ok := output.Value().([]byte)
	if !ok {
		return malformed("answered the session handshake with %s", output.Signature())
	}
	server := new(big.Int).SetBytes(raw)
	if server.Cmp(big.NewInt(1)) <= 0 || server.Cmp(new(big.Int).Sub(prime, big.NewInt(1))) >= 0 {
		return malformed("answered the session handshake with a public key outside the group")
	}
	shared := new(big.Int).Exp(server, priv, prime).Bytes()
	padded := make([]byte, 0x80)
	copy(padded[len(padded)-len(shared):], shared)
	key, err := hkdf.Key(sha256.New, padded, nil, "", aes.BlockSize)
	if err != nil {
		return err
	}
	s.session, s.aesKey = path, key
	return nil
}

// Secret struct carrying value, AES-128-CBC encrypted with PKCS7 padding inside an encrypted session
func (s *secretService) encode(value []byte) (ssSecret, error) {
	if s.aesKey == nil {
		return ssSecret{Session: s.session, Parameters: []byte{}, Value: value, ContentType: "text/plain"}, nil
	}
	pad := aes.BlockSize - len(value)%aes.BlockSize
	padded := append(slices.Clone(value), bytes.Repeat([]byte{byte(pad)}, pad)...)
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return ssSecret{}, err
	}
	block, err := aes.NewCipher(s.aesKey)
	if err != nil {
		return ssSecret{}, err
	}
	sealed := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(sealed, padded)
	return ssSecret{Session: s.session, Parameters: iv, Value: sealed, ContentType: "text/plain"}, nil
}

// Value of a transferred secret, decrypted and its PKCS7 padding verified when the session is encrypted
func (s *secretService) decode(sec ssSecret) ([]byte, error) {
	if s.aesKey == nil {
		return sec.Value, nil
	}
	if len(sec.Parameters) != aes.BlockSize || len(sec.Value) == 0 || len(sec.Value)%aes.BlockSize != 0 {
		return nil, malformed("returned a malformed encrypted secret")
	}
	block, err := aes.NewCipher(s.aesKey)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(sec.Value))
	cipher.NewCBCDecrypter(block, sec.Parameters).CryptBlocks(plain, sec.Value)
	pad, ok := pkcs7Padding(plain)
	if !ok {
		return nil, malformed("returned an encrypted secret whose padding does not verify")
	}
	return plain[:len(plain)-pad], nil
}

// Items of collection whose service and username attributes match
func (s *secretService) search(ctx context.Context, collection dbus.ObjectPath, service, account string) ([]dbus.ObjectPath, error) {
	var items []dbus.ObjectPath
	attrs := map[string]string{"username": account, "service": service}
	if err := s.call(ctx, collection, ssCollection+".SearchItems", attrs).Store(&items); err != nil {
		return nil, ssError(err)
	}
	return items, nil
}

// Reports whether the Secret Service daemon owns its bus name or can be activated
func (k *Keychain) check(ctx context.Context) error {
	ss, err := dialSecretService(ctx)
	if err != nil {
		return err
	}
	defer ss.close()
	var owned bool
	if err := ss.busCall(ctx, "org.freedesktop.DBus.NameHasOwner", ssBus).Store(&owned); err != nil {
		return noKeychain(fmt.Sprintf("the session bus did not answer: %v", err))
	}
	if owned {
		return nil
	}
	var names []string
	if err := ss.busCall(ctx, "org.freedesktop.DBus.ListActivatableNames").Store(&names); err != nil {
		return noKeychain(fmt.Sprintf("the session bus did not answer: %v", err))
	}
	if slices.Contains(names, ssBus) {
		return nil
	}
	return noKeychain("the Secret Service daemon is neither running nor activatable through D-Bus")
}

// Secret of the first matching item in the default collection, unlocking it first
func (k *Keychain) get(ctx context.Context, service, account string) (string, error) {
	ss, err := dialSecretService(ctx)
	if err != nil {
		return "", err
	}
	defer ss.close()
	collection, err := ss.defaultCollection(ctx)
	if err != nil {
		return "", err
	}
	items, err := ss.search(ctx, collection, service, account)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", keychainError(ErrNotFound, "no Secret Service item for %s / %s", quote(service), quote(account))
	}
	item := items[0]
	if err := ss.unlock(ctx, "the Secret Service item unlock prompt", item); err != nil {
		return "", err
	}
	locked, err := ss.locked(ctx, item, ssItem)
	if err != nil {
		return "", err
	}
	if locked {
		return "", keychainError(ErrDenied, "Failed to unlock the item!")
	}
	if err := ss.openSession(ctx); err != nil {
		return "", err
	}
	var sec ssSecret
	if err := ss.call(ctx, item, ssItem+".GetSecret", ss.session).Store(&sec); err != nil {
		return "", ssError(err)
	}
	value, err := ss.decode(sec)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(value) {
		return "", keychainError(errSecretServiceReply, "the Secret Service item holds a secret that is not UTF-8 text")
	}
	return string(value), nil
}

// Creates or replaces the item for service and account, leaving it the only match in the default collection
func (k *Keychain) set(ctx context.Context, service, account, secret string) error {
	ss, err := dialSecretService(ctx)
	if err != nil {
		return err
	}
	defer ss.close()
	collection, err := ss.defaultCollection(ctx)
	if err != nil {
		return err
	}
	locked, err := ss.locked(ctx, collection, ssCollection)
	if err != nil {
		return err
	}
	if locked {
		return keychainError(ErrDenied, "Collection is locked!")
	}
	if err := ss.openSession(ctx); err != nil {
		return err
	}
	sec, err := ss.encode([]byte(secret))
	if err != nil {
		return err
	}
	props := map[string]dbus.Variant{
		ssItem + ".Label": dbus.MakeVariant(fmt.Sprintf("Password for '%s' on '%s'", account, service)),
		ssItem + ".Attributes": dbus.MakeVariant(map[string]string{"application": ssApplication, "service": service,
			"username": account, "xdg:schema": ssSchema}),
	}
	var item, prompt dbus.ObjectPath
	if err := ss.call(ctx, collection, ssCollection+".CreateItem", props, sec, true).Store(&item, &prompt); err != nil {
		return ssError(err)
	}
	if len(item) <= 1 {
		dismissed, result, err := ss.prompt(ctx, prompt, "the Secret Service prompt storing the item")
		if err != nil {
			return err
		}
		if dismissed {
			return keychainError(ErrDenied, "Prompt dismissed.")
		}
		if item, err = promptPath(result); err != nil {
			return err
		}
	}
	items, err := ss.search(ctx, collection, service, account)
	if err != nil {
		return err
	}
	for _, other := range items {
		if other != item {
			if err := ss.deleteItem(ctx, other); err != nil {
				return err
			}
		}
	}
	return nil
}

// Deletes one item, refusing a locked one and confirming a prompt the service asks for
func (s *secretService) deleteItem(ctx context.Context, item dbus.ObjectPath) error {
	locked, err := s.locked(ctx, item, ssItem)
	if err != nil {
		return err
	}
	if locked {
		return keychainError(ErrDenied, "Item is locked!")
	}
	var prompt dbus.ObjectPath
	if err := s.call(ctx, item, ssItem+".Delete").Store(&prompt); err != nil {
		return ssError(err)
	}
	if len(prompt) <= 1 {
		return nil
	}
	dismissed, _, err := s.prompt(ctx, prompt, "the Secret Service prompt deleting the item")
	if err != nil {
		return err
	}
	if dismissed {
		return keychainError(ErrDenied, "Prompt dismissed.")
	}
	return nil
}

// Deletes the first matching item in the default collection
func (k *Keychain) del(ctx context.Context, service, account string) error {
	ss, err := dialSecretService(ctx)
	if err != nil {
		return err
	}
	defer ss.close()
	collection, err := ss.defaultCollection(ctx)
	if err != nil {
		return err
	}
	items, err := ss.search(ctx, collection, service, account)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return keychainError(ErrNotFound, "No such password!")
	}
	return ss.deleteItem(ctx, items[0])
}
