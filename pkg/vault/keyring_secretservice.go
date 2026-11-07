package vault

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
)

const (
	ssBusName      = "org.freedesktop.secrets"
	ssPath         = dbusObjectPath("/org/freedesktop/secrets")
	ssServiceIface = "org.freedesktop.Secret.Service"
	ssCollIface    = "org.freedesktop.Secret.Collection"
	ssItemIface    = "org.freedesktop.Secret.Item"
	ssPromptIface  = "org.freedesktop.Secret.Prompt"
	ssDefaultColl  = dbusObjectPath("/org/freedesktop/secrets/aliases/default")
	ssAlgorithmDH  = "dh-ietf1024-sha256-aes128-cbc-pkcs7"
	ssAppID        = "Python keyring library"
)

// RFC 2409 second Oakley group prime secretstorage negotiates the session key over
var ssDHPrime, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B2"+
	"2514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6"+
	"F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE65381FFFFFFFFFFFFFFFF", 16)

// Freedesktop Secret Service keyring over the session bus, secretstorage's protocol in Go
type secretServiceKeyring struct {
	dial func() (*dbusConn, error)
}

func newSecretServiceKeyring() secretServiceKeyring {
	return secretServiceKeyring{dial: dbusDialSession}
}

func (secretServiceKeyring) keyringModule() string { return "keyring.backends.SecretService" }
func (secretServiceKeyring) keyringClass() string  { return "Keyring" }

// Error when the daemon neither owns its bus name nor is activatable, keyring's priority check
func (k secretServiceKeyring) viable() error {
	c, err := k.dial()
	if err != nil {
		return err
	}
	defer c.Close()
	out, err := c.call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "NameHasOwner", "s", ssBusName)
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
	names, _ := out[0].([]any)
	for _, n := range names {
		if n == ssBusName {
			return nil
		}
	}
	return errors.New("The Secret Service daemon is neither running nor activatable through D-Bus")
}

type ssSession struct {
	path      dbusObjectPath
	aesKey    []byte
	encrypted bool
}

// Maps the D-Bus errors secretstorage turns into its own exception classes
func ssError(err error) error {
	var de *dbusError
	if !errors.As(err, &de) {
		return err
	}
	switch de.Name {
	case "org.freedesktop.DBus.Error.UnknownMethod", "org.freedesktop.Secret.Error.NoSuchObject":
		return &keyringError{"ItemNotFoundException", "Item does not exist!"}
	case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.Spawn.ExecFailed",
		"org.freedesktop.DBus.Error.NoReply", "org.freedesktop.DBus.Error.Spawn.ChildExited":
		return &keyringError{"SecretServiceNotAvailableException", "Secret Service is not available: " + de.Error()}
	}
	return err
}

func ssCall(c *dbusConn, path dbusObjectPath, iface, member, sig string, args ...any) ([]any, error) {
	out, err := c.call(ssBusName, path, iface, member, sig, args...)
	return out, ssError(err)
}

func ssProperty(c *dbusConn, path dbusObjectPath, iface, name string) (any, error) {
	out, err := ssCall(c, path, "org.freedesktop.DBus.Properties", "Get", "ss", iface, name)
	if err != nil {
		return nil, err
	}
	return out[0].(dbusVariant).Val, nil
}

func ssLocked(c *dbusConn, path dbusObjectPath, iface string) (bool, error) {
	v, err := ssProperty(c, path, iface, "Locked")
	locked, _ := v.(bool)
	return locked, err
}

// Runs a prompt and waits for Completed, returning whether the user dismissed it and its result
func ssExecPrompt(c *dbusConn, prompt dbusObjectPath) (bool, dbusVariant, error) {
	rule := fmt.Sprintf("type='signal',interface='%s',member='Completed',path='%s'", ssPromptIface, prompt)
	if _, err := c.call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "AddMatch", "s", rule); err != nil {
		return false, dbusVariant{}, err
	}
	if _, err := ssCall(c, prompt, ssPromptIface, "Prompt", "s", ""); err != nil {
		return false, dbusVariant{}, err
	}
	sig, err := c.waitSignal(prompt, ssPromptIface, "Completed")
	if err != nil || len(sig.Body) < 2 {
		return false, dbusVariant{}, err
	}
	dismissed, _ := sig.Body[0].(bool)
	result, _ := sig.Body[1].(dbusVariant)
	return dismissed, result, nil
}

// Unlocks objects through the service, true when the prompt was dismissed
func ssUnlock(c *dbusConn, paths ...dbusObjectPath) (bool, error) {
	out, err := ssCall(c, ssPath, ssServiceIface, "Unlock", "ao", paths)
	if err != nil {
		return false, err
	}
	if prompt, _ := out[1].(dbusObjectPath); len(prompt) > 1 {
		dismissed, _, err := ssExecPrompt(c, prompt)
		return dismissed, err
	}
	return false, nil
}

// Opens a DH encrypted transfer session, plain when the service answers NotSupported
func ssOpenSession(c *dbusConn) (*ssSession, error) {
	privBytes := make([]byte, 0x80)
	rand.Read(privBytes)
	priv := new(big.Int).SetBytes(privBytes)
	pub := new(big.Int).Exp(big.NewInt(2), priv, ssDHPrime)
	out, err := c.call(ssBusName, ssPath, ssServiceIface, "OpenSession", "sv", ssAlgorithmDH, dbusVariant{"ay", pub.Bytes()})
	var de *dbusError
	if errors.As(err, &de) && de.Name == "org.freedesktop.DBus.Error.NotSupported" {
		out, err = ssCall(c, ssPath, ssServiceIface, "OpenSession", "sv", "plain", dbusVariant{"s", ""})
		if err != nil {
			return nil, err
		}
		path, _ := out[1].(dbusObjectPath)
		return &ssSession{path: path}, nil
	}
	if err != nil {
		return nil, ssError(err)
	}
	output, _ := out[0].(dbusVariant)
	serverPub, ok := output.Val.([]byte)
	if !ok || output.Sig != "ay" {
		return nil, errors.New("AssertionError")
	}
	common := new(big.Int).Exp(new(big.Int).SetBytes(serverPub), priv, ssDHPrime).Bytes()
	common = append(bytes.Repeat([]byte{0}, 0x80-len(common)), common...)
	prk := hmac.New(sha256.New, make([]byte, 0x20))
	prk.Write(common)
	okm := hmac.New(sha256.New, prk.Sum(nil))
	okm.Write([]byte{1})
	path, _ := out[1].(dbusObjectPath)
	return &ssSession{path: path, aesKey: okm.Sum(nil)[:0x10], encrypted: true}, nil
}

// Secret struct for a value, AES CBC with PKCS7 padding inside an encrypted session
func (s *ssSession) format(secret []byte) []any {
	if !s.encrypted {
		return []any{s.path, []byte{}, secret, "text/plain"}
	}
	pad := 0x10 - len(secret)&0xf
	padded := append(append([]byte{}, secret...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	iv := make([]byte, 0x10)
	rand.Read(iv)
	block, _ := aes.NewCipher(s.aesKey)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	return []any{s.path, iv, ct, "text/plain"}
}

func (s *ssSession) open(secret []any) ([]byte, error) {
	value, _ := secret[2].([]byte)
	if !s.encrypted {
		return value, nil
	}
	iv, _ := secret[1].([]byte)
	if len(iv) != 0x10 || len(value) == 0 || len(value)%0x10 != 0 {
		return nil, errors.New("ValueError: malformed encrypted secret")
	}
	block, _ := aes.NewCipher(s.aesKey)
	plain := make([]byte, len(value))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, value)
	pad := int(plain[len(plain)-1])
	if pad > len(plain) {
		return nil, errors.New("ValueError: bad padding")
	}
	return plain[:len(plain)-pad], nil
}

// Default collection, created when missing and unlocked, KeyringLocked when the prompt is dismissed
func (k secretServiceKeyring) collection(c *dbusConn) (dbusObjectPath, error) {
	path := ssDefaultColl
	if _, err := ssProperty(c, path, ssCollIface, "Label"); err != nil {
		var ke *keyringError
		if !errors.As(err, &ke) || ke.Class != "ItemNotFoundException" {
			return "", &keyringError{"InitError", fmt.Sprintf("Failed to create the collection: %s.", err)}
		}
		props := []dbusEntry{{"org.freedesktop.Secret.Collection.Label", dbusVariant{"s", "Default"}}}
		out, err := ssCall(c, ssPath, ssServiceIface, "CreateCollection", "a{sv}s", props, "default")
		if err != nil {
			return "", &keyringError{"InitError", fmt.Sprintf("Failed to create the collection: %s.", err)}
		}
		path, _ = out[0].(dbusObjectPath)
		if len(path) <= 1 {
			dismissed, result, err := ssExecPrompt(c, out[1].(dbusObjectPath))
			if err != nil {
				return "", &keyringError{"InitError", fmt.Sprintf("Failed to create the collection: %s.", err)}
			}
			if dismissed {
				return "", &keyringError{"InitError", "Failed to create the collection: Prompt dismissed.."}
			}
			path, _ = result.Val.(dbusObjectPath)
		}
	}
	locked, err := ssLocked(c, path, ssCollIface)
	if err != nil {
		return "", err
	}
	if locked {
		if _, err := ssUnlock(c, path); err != nil {
			return "", err
		}
		if locked, err = ssLocked(c, path, ssCollIface); err != nil {
			return "", err
		}
		if locked {
			return "", &keyringError{"KeyringLocked", "Failed to unlock the collection!"}
		}
	}
	return path, nil
}

func (k secretServiceKeyring) search(c *dbusConn, service, account string) (dbusObjectPath, []any, error) {
	coll, err := k.collection(c)
	if err != nil {
		return "", nil, err
	}
	out, err := ssCall(c, coll, ssCollIface, "SearchItems", "a{ss}", map[string]string{"username": account, "service": service})
	if err != nil {
		return "", nil, err
	}
	items, _ := out[0].([]any)
	return coll, items, nil
}

func (k secretServiceKeyring) GetPassword(service, account string) (string, bool, error) {
	c, err := k.dial()
	if err != nil {
		return "", false, err
	}
	defer c.Close()
	_, items, err := k.search(c, service, account)
	if err != nil || len(items) == 0 {
		return "", false, err
	}
	item := items[0].(dbusObjectPath)
	if _, err := ssUnlock(c, item); err != nil {
		return "", false, err
	}
	if locked, err := ssLocked(c, item, ssItemIface); err != nil || locked {
		if err == nil {
			err = &keyringError{"KeyringLocked", "Failed to unlock the item!"}
		}
		return "", false, err
	}
	session, err := ssOpenSession(c)
	if err != nil {
		return "", false, err
	}
	out, err := ssCall(c, item, ssItemIface, "GetSecret", "o", session.path)
	if err != nil {
		return "", false, err
	}
	value, err := session.open(out[0].([]any))
	if err != nil {
		return "", false, err
	}
	return string(value), true, nil
}

func (k secretServiceKeyring) SetPassword(service, account, password string) error {
	c, err := k.dial()
	if err != nil {
		return err
	}
	defer c.Close()
	coll, err := k.collection(c)
	if err != nil {
		return err
	}
	if locked, err := ssLocked(c, coll, ssCollIface); err != nil || locked {
		if err == nil {
			err = &keyringError{"LockedException", "Collection is locked!"}
		}
		return err
	}
	session, err := ssOpenSession(c)
	if err != nil {
		return err
	}
	attrs := map[string]string{"application": ssAppID, "service": service, "username": account}
	props := []dbusEntry{
		{"org.freedesktop.Secret.Item.Label", dbusVariant{"s", fmt.Sprintf("Password for '%s' on '%s'", account, service)}},
		{"org.freedesktop.Secret.Item.Attributes", dbusVariant{"a{ss}", attrs}},
	}
	out, err := ssCall(c, coll, ssCollIface, "CreateItem", "a{sv}(oayays)b", props, session.format([]byte(password)), true)
	if err != nil {
		return err
	}
	if item, _ := out[0].(dbusObjectPath); len(item) > 1 {
		return nil
	}
	dismissed, _, err := ssExecPrompt(c, out[1].(dbusObjectPath))
	if err == nil && dismissed {
		err = &keyringError{"PromptDismissedException", "Prompt dismissed."}
	}
	return err
}

func (k secretServiceKeyring) DeletePassword(service, account string) error {
	c, err := k.dial()
	if err != nil {
		return err
	}
	defer c.Close()
	_, items, err := k.search(c, service, account)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return &keyringError{"PasswordDeleteError", "No such password!"}
	}
	item := items[0].(dbusObjectPath)
	if locked, err := ssLocked(c, item, ssItemIface); err != nil || locked {
		if err == nil {
			err = &keyringError{"LockedException", "Item is locked!"}
		}
		return err
	}
	out, err := ssCall(c, item, ssItemIface, "Delete", "")
	if err != nil {
		return err
	}
	if prompt, _ := out[0].(dbusObjectPath); prompt != "/" {
		dismissed, _, err := ssExecPrompt(c, prompt)
		if err == nil && dismissed {
			err = &keyringError{"PromptDismissedException", "Prompt dismissed."}
		}
		return err
	}
	return nil
}
