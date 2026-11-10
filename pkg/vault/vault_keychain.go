package vault

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Keychain service every vault key entry is filed under
const VaultKeychainService = "Icebath Agent vault key"

// keyring backends that only move the key into another file under the same home
var notAKeychain = []string{"keyring.backends.fail", "keyring.backends.null", "keyrings.alt"}

// No OS keychain is reachable from this process, headless or a plaintext backend
type KeychainUnavailable struct{ Msg string }

func (e *KeychainUnavailable) Error() string { return e.Msg }

func isOSKeychain(b keyringBackend) bool {
	if ch, ok := b.(keyringChainer); ok {
		kids := ch.keyringChildren()
		return len(kids) > 0 && isOSKeychain(kids[0])
	}
	for _, p := range notAKeychain {
		if strings.HasPrefix(b.keyringModule(), p) {
			return false
		}
	}
	return true
}

// OS credential store keyring resolves to, KeychainUnavailable for none or a plaintext one
func osKeyring() (keyringBackend, error) {
	b, err := keyringGetKeyring()
	if err != nil {
		return nil, &KeychainUnavailable{fmt.Sprintf("keyring could not pick a backend (%s)", pyErrorClass(err))}
	}
	if !isOSKeychain(b) {
		return nil, &KeychainUnavailable{fmt.Sprintf("no OS keychain is reachable from this session (keyring backend: %s.%s)",
			b.keyringModule(), b.keyringClass())}
	}
	return b, nil
}

// Human name of the keychain this process would use, for the vault key command
func KeychainBackendLabel() string {
	b, err := osKeyring()
	if err != nil {
		return "unavailable: " + err.Error()
	}
	return keyringName(b)
}

func resolvedDir(dir string) string {
	if r, err := pypath.Resolve(dir); err == nil {
		return r
	}
	abs, _ := filepath.Abs(dir)
	return abs
}

// Coordinates for a new entry with a random account, recording which directory owns it
func vaultNewRef(vaultDir string) *pyjson.Object {
	return pyjson.ObjectOf("storage", "keychain", "service", VaultKeychainService,
		"account", "vault-"+strings.ReplaceAll(uuid.NewString(), "-", ""), "vault_dir", resolvedDir(vaultDir))
}

func refString(ref *pyjson.Object, key string) string {
	v, _ := ref.Get(key)
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

// False for a pointer copied or moved from another directory, whose entry may be another vault's only key
func vaultOwnsRef(ref *pyjson.Object, vaultDir string) bool {
	v, _ := ref.Get("vault_dir")
	s, ok := v.(string)
	return ok && s == resolvedDir(vaultDir)
}

// Keychain pointer saved at path, nil when absent and a VaultError when unreadable or not a pointer
func vaultReadRef(path string) (*pyjson.Object, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var data any
	if err == nil {
		text, derr := pyjson.DecodeBytes([]byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")))
		if derr != nil {
			err = &keyringError{"UnicodeDecodeError", derr.Error()}
		} else if data, derr = pyjson.Loads(text); derr != nil {
			err = &keyringError{"JSONDecodeError", derr.Error()}
		}
	}
	if err != nil {
		return nil, &VaultError{fmt.Sprintf("%s is unreadable (%s); see `icebath vault key`", filepath.Base(path), pyErrorClass(err))}
	}
	obj, ok := data.(*pyjson.Object)
	if !ok || refString(obj, "storage") != "keychain" {
		return nil, &VaultError{filepath.Base(path) + " does not describe a keychain entry; see `icebath vault key`"}
	}
	if acct, _ := obj.Get("account"); !pyrepr.Truthy(acct) {
		return nil, &VaultError{filepath.Base(path) + " does not describe a keychain entry; see `icebath vault key`"}
	}
	return obj, nil
}

// Saves a pointer atomically with a durable directory entry, swapped by tests for a failing writer
var vaultWriteRef = func(ctx context.Context, path string, ref *pyjson.Object) error {
	text, err := pyjson.Dumps(ref, pyjson.Indent(2))
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	return pyfs.AtomicWriteBytes(ctx, path, []byte(text), "", &mode, true)
}

// Saves the pointer to a fresh entry, deleting the entry unless the pointer landed before the failure
func vaultSaveNewRef(ctx context.Context, path string, ref *pyjson.Object) error {
	err := vaultWriteRef(ctx, path, ref)
	if err == nil {
		return nil
	}
	if saved, rerr := vaultReadRef(path); rerr == nil && saved != nil && refString(saved, "account") == refString(ref, "account") {
		return err
	}
	vaultDeleteKey(ref)
	return err
}

func refCoords(ref *pyjson.Object) (string, string) {
	service := refString(ref, "service")
	if service == "" {
		service = VaultKeychainService
	}
	return service, refString(ref, "account")
}

// Stored key or nil when the entry is gone, KeychainUnavailable when the keychain cannot answer
func vaultLoadKey(ref *pyjson.Object) ([]byte, error) {
	b, err := osKeyring()
	if err != nil {
		return nil, err
	}
	service, account := refCoords(ref)
	value, ok, err := b.GetPassword(service, account)
	if err != nil {
		return nil, &KeychainUnavailable{fmt.Sprintf("the OS keychain did not answer (%s)", pyErrorClass(err))}
	}
	if !ok || value == "" {
		return nil, nil
	}
	return []byte(value), nil
}

// Writes the key and reads it back, a store returning something else being unusable
func vaultStoreKey(ref *pyjson.Object, key []byte) error {
	b, err := osKeyring()
	if err != nil {
		return err
	}
	service, account := refCoords(ref)
	if err := b.SetPassword(service, account, string(key)); err != nil {
		return &KeychainUnavailable{fmt.Sprintf("the OS keychain refused the key (%s)", pyErrorClass(err))}
	}
	readback, _, err := b.GetPassword(service, account)
	if err != nil {
		return &KeychainUnavailable{fmt.Sprintf("the OS keychain refused the key (%s)", pyErrorClass(err))}
	}
	if readback != string(key) {
		return &KeychainUnavailable{"the OS keychain did not return the key it was given"}
	}
	return nil
}

// Deletes the entry a plain map pointer names, true once it is gone
func VaultDeleteKeyRef(ref map[string]any) bool {
	obj, ok := pyjson.AsObject(ref)
	if !ok {
		obj = pyjson.NewObject()
	}
	return vaultDeleteKey(obj)
}

// True once the entry is gone, including after an interrupted cleanup already removed it
func vaultDeleteKey(ref *pyjson.Object) bool {
	service, account := refCoords(ref)
	b, err := osKeyring()
	if err != nil {
		return false
	}
	b.DeletePassword(service, account)
	_, ok, err := b.GetPassword(service, account)
	return err == nil && !ok
}

func vaultRemoveFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
