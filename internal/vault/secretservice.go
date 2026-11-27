//go:build !darwin && !windows

package vault

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"slices"
)

const (
	ssBus         = "org.freedesktop.secrets"
	ssPath        = objectPath("/org/freedesktop/secrets")
	ssService     = "org.freedesktop.Secret.Service"
	ssCollection  = "org.freedesktop.Secret.Collection"
	ssItem        = "org.freedesktop.Secret.Item"
	ssPrompt      = "org.freedesktop.Secret.Prompt"
	ssDefault     = objectPath("/org/freedesktop/secrets/aliases/default")
	ssAlgorithm   = "dh-ietf1024-sha256-aes128-cbc-pkcs7"
	ssApplication = "icebath"
)

// Second Oakley group prime of RFC 2409, which the transfer session's key is agreed over
var ssPrime, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B2"+
	"2514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6"+
	"F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE65381FFFFFFFFFFFFFFFF", 16)

// The freedesktop Secret Service over the session bus, the default collection holding the entries
type secretService struct{}

func nativeKeyring() (keyring, error) {
	if err := viable(); err != nil {
		return nil, fmt.Errorf("no OS keychain is reachable from this session: %v", err)
	}
	return secretService{}, nil
}

func (secretService) name() string { return "Secret Service" }

// Why the Secret Service can't serve, nil when its daemon owns its name or can be started
func viable() error {
	c, err := dialSession()
	if err != nil {
		return err
	}
	defer c.Close()
	out, err := c.call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "NameHasOwner", "s", ssBus)
	if err != nil {
		return err
	}
	if owned, _ := out[0].(bool); owned {
		return nil
	}
	out, err = c.call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "ListActivatableNames", "")
	if err != nil {
		return err
	}
	if names, _ := out[0].([]any); slices.Contains(names, any(ssBus)) {
		return nil
	}
	return errors.New("The Secret Service daemon is neither running nor activatable through D-Bus")
}

// Failures of the Secret Service under the classes they mean
func ssError(err error) error {
	var be *busError
	if !errors.As(err, &be) {
		return err
	}
	switch be.name {
	case "org.freedesktop.DBus.Error.UnknownMethod", "org.freedesktop.Secret.Error.NoSuchObject":
		return &keyringError{"ItemNotFoundException", "Item does not exist!"}
	case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.Spawn.ExecFailed", "org.freedesktop.DBus.Error.NoReply",
		"org.freedesktop.DBus.Error.Spawn.ChildExited":
		return &keyringError{"SecretServiceNotAvailableException", "Secret Service is not available: " + be.Error()}
	}
	return err
}

func ssCall(c *busConn, path objectPath, iface, member, sig string, args ...any) ([]any, error) {
	out, err := c.call(ssBus, path, iface, member, sig, args...)
	return out, ssError(err)
}

func ssProperty(c *busConn, path objectPath, iface, name string) (any, error) {
	out, err := ssCall(c, path, "org.freedesktop.DBus.Properties", "Get", "ss", iface, name)
	if err != nil {
		return nil, err
	}
	return out[0].(busVariant).val, nil
}

func ssLocked(c *busConn, path objectPath, iface string) (bool, error) {
	v, err := ssProperty(c, path, iface, "Locked")
	locked, _ := v.(bool)
	return locked, err
}

// Runs a prompt and waits for it to complete, answering whether the user dismissed it and its result
func ssExecPrompt(c *busConn, prompt objectPath) (bool, busVariant, error) {
	rule := fmt.Sprintf("type='signal',interface='%s',member='Completed',path='%s'", ssPrompt, prompt)
	if _, err := c.call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "AddMatch", "s", rule); err != nil {
		return false, busVariant{}, err
	}
	if _, err := ssCall(c, prompt, ssPrompt, "Prompt", "s", ""); err != nil {
		return false, busVariant{}, err
	}
	sig, err := c.waitSignal(prompt, ssPrompt, "Completed")
	if err != nil || len(sig.body) < 2 {
		return false, busVariant{}, err
	}
	dismissed, _ := sig.body[0].(bool)
	result, _ := sig.body[1].(busVariant)
	return dismissed, result, nil
}

// Unlocks objects through the service, answering whether its prompt was dismissed
func ssUnlock(c *busConn, paths ...objectPath) (bool, error) {
	out, err := ssCall(c, ssPath, ssService, "Unlock", "ao", paths)
	if err != nil {
		return false, err
	}
	if prompt, _ := out[1].(objectPath); len(prompt) > 1 {
		dismissed, _, err := ssExecPrompt(c, prompt)
		return dismissed, err
	}
	return false, nil
}

// Session secrets cross the bus in: AES encrypted under a key agreed by Diffie-Hellman, or plain where the service supports nothing else
type ssSession struct {
	path      objectPath
	key       []byte
	encrypted bool
}

func ssOpenSession(c *busConn) (*ssSession, error) {
	secret := make([]byte, 0x80)
	rand.Read(secret)
	private := new(big.Int).SetBytes(secret)
	public := new(big.Int).Exp(big.NewInt(2), private, ssPrime)
	out, err := c.call(ssBus, ssPath, ssService, "OpenSession", "sv", ssAlgorithm, busVariant{"ay", public.Bytes()})
	var be *busError
	if errors.As(err, &be) && be.name == "org.freedesktop.DBus.Error.NotSupported" {
		if out, err = ssCall(c, ssPath, ssService, "OpenSession", "sv", "plain", busVariant{"s", ""}); err != nil {
			return nil, err
		}
		path, _ := out[1].(objectPath)
		return &ssSession{path: path}, nil
	}
	if err != nil {
		return nil, ssError(err)
	}
	output, _ := out[0].(busVariant)
	theirs, ok := output.val.([]byte)
	if !ok || output.sig != "ay" {
		return nil, errors.New("the Secret Service answered the key exchange with no public key")
	}
	shared := new(big.Int).Exp(new(big.Int).SetBytes(theirs), private, ssPrime).Bytes()
	shared = append(bytes.Repeat([]byte{0}, 0x80-len(shared)), shared...)
	key, err := hkdf.Key(sha256.New, shared, make([]byte, 0x20), "", 0x10)
	if err != nil {
		return nil, err
	}
	path, _ := out[1].(objectPath)
	return &ssSession{path: path, key: key, encrypted: true}, nil
}

// Secret struct of a value, AES-CBC with PKCS7 padding inside an encrypted session
func (s *ssSession) format(secret []byte) []any {
	if !s.encrypted {
		return []any{s.path, []byte{}, secret, "text/plain"}
	}
	pad := 0x10 - len(secret)&0xf
	padded := append(append([]byte{}, secret...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	iv := make([]byte, 0x10)
	rand.Read(iv)
	block, _ := aes.NewCipher(s.key)
	sealed := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(sealed, padded)
	return []any{s.path, iv, sealed, "text/plain"}
}

func (s *ssSession) open(secret []any) ([]byte, error) {
	value, _ := secret[2].([]byte)
	if !s.encrypted {
		return value, nil
	}
	iv, _ := secret[1].([]byte)
	if len(iv) != 0x10 || len(value) == 0 || len(value)%0x10 != 0 {
		return nil, errors.New("the Secret Service returned a malformed encrypted secret")
	}
	block, _ := aes.NewCipher(s.key)
	plain := make([]byte, len(value))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, value)
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > len(plain) {
		return nil, errors.New("the Secret Service secret has bad padding")
	}
	return plain[:len(plain)-pad], nil
}

// Default collection, made when it is missing and unlocked when it is locked
func ssDefaultCollection(c *busConn) (objectPath, error) {
	path := ssDefault
	if _, err := ssProperty(c, path, ssCollection, "Label"); err != nil {
		var ke *keyringError
		if !errors.As(err, &ke) || ke.class != "ItemNotFoundException" {
			return "", &keyringError{"InitError", fmt.Sprintf("Failed to create the collection: %s.", err)}
		}
		props := []busEntry{{"org.freedesktop.Secret.Collection.Label", busVariant{"s", "Default"}}}
		out, err := ssCall(c, ssPath, ssService, "CreateCollection", "a{sv}s", props, "default")
		if err != nil {
			return "", &keyringError{"InitError", fmt.Sprintf("Failed to create the collection: %s.", err)}
		}
		path, _ = out[0].(objectPath)
		if len(path) <= 1 {
			dismissed, result, err := ssExecPrompt(c, out[1].(objectPath))
			switch {
			case err != nil:
				return "", &keyringError{"InitError", fmt.Sprintf("Failed to create the collection: %s.", err)}
			case dismissed:
				return "", &keyringError{"InitError", "Failed to create the collection: Prompt dismissed.."}
			}
			path, _ = result.val.(objectPath)
		}
	}
	locked, err := ssLocked(c, path, ssCollection)
	if err != nil {
		return "", err
	}
	if locked {
		if _, err := ssUnlock(c, path); err != nil {
			return "", err
		}
		if locked, err = ssLocked(c, path, ssCollection); err != nil {
			return "", err
		}
		if locked {
			return "", &keyringError{"KeyringLocked", "Failed to unlock the collection!"}
		}
	}
	return path, nil
}

func ssSearch(c *busConn, service, account string) ([]any, error) {
	collection, err := ssDefaultCollection(c)
	if err != nil {
		return nil, err
	}
	out, err := ssCall(c, collection, ssCollection, "SearchItems", "a{ss}", map[string]string{"username": account, "service": service})
	if err != nil {
		return nil, err
	}
	items, _ := out[0].([]any)
	return items, nil
}

func (secretService) get(service, account string) (string, bool, error) {
	c, err := dialSession()
	if err != nil {
		return "", false, err
	}
	defer c.Close()
	items, err := ssSearch(c, service, account)
	if err != nil || len(items) == 0 {
		return "", false, err
	}
	item := items[0].(objectPath)
	if _, err := ssUnlock(c, item); err != nil {
		return "", false, err
	}
	if locked, err := ssLocked(c, item, ssItem); err != nil || locked {
		if err == nil {
			err = &keyringError{"KeyringLocked", "Failed to unlock the item!"}
		}
		return "", false, err
	}
	session, err := ssOpenSession(c)
	if err != nil {
		return "", false, err
	}
	out, err := ssCall(c, item, ssItem, "GetSecret", "o", session.path)
	if err != nil {
		return "", false, err
	}
	value, err := session.open(out[0].([]any))
	if err != nil {
		return "", false, err
	}
	return string(value), true, nil
}

func (secretService) set(service, account, secret string) error {
	c, err := dialSession()
	if err != nil {
		return err
	}
	defer c.Close()
	collection, err := ssDefaultCollection(c)
	if err != nil {
		return err
	}
	session, err := ssOpenSession(c)
	if err != nil {
		return err
	}
	attributes := map[string]string{"application": ssApplication, "service": service, "username": account}
	props := []busEntry{
		{"org.freedesktop.Secret.Item.Label", busVariant{"s", fmt.Sprintf("Password for '%s' on '%s'", account, service)}},
		{"org.freedesktop.Secret.Item.Attributes", busVariant{"a{ss}", attributes}},
	}
	out, err := ssCall(c, collection, ssCollection, "CreateItem", "a{sv}(oayays)b", props, session.format([]byte(secret)), true)
	if err != nil {
		return err
	}
	if item, _ := out[0].(objectPath); len(item) > 1 {
		return nil
	}
	dismissed, _, err := ssExecPrompt(c, out[1].(objectPath))
	if err == nil && dismissed {
		err = &keyringError{"PromptDismissedException", "Prompt dismissed."}
	}
	return err
}

func (secretService) remove(service, account string) error {
	c, err := dialSession()
	if err != nil {
		return err
	}
	defer c.Close()
	items, err := ssSearch(c, service, account)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return &keyringError{"PasswordDeleteError", "No such password!"}
	}
	item := items[0].(objectPath)
	if locked, err := ssLocked(c, item, ssItem); err != nil || locked {
		if err == nil {
			err = &keyringError{"LockedException", "Item is locked!"}
		}
		return err
	}
	out, err := ssCall(c, item, ssItem, "Delete", "")
	if err != nil {
		return err
	}
	if prompt, _ := out[0].(objectPath); prompt != "/" {
		dismissed, _, err := ssExecPrompt(c, prompt)
		if err == nil && dismissed {
			err = &keyringError{"PromptDismissedException", "Prompt dismissed."}
		}
		return err
	}
	return nil
}
