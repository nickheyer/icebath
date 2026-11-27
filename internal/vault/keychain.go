package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Where a vault key lives in the OS keychain, kept in vault.keyref beside the vault
type keyRef struct {
	Storage string `json:"storage"`
	Service string `json:"service"`
	Account string `json:"account"`
	// Folder of the vault the entry was made for, so a copied vault never deletes another's only key
	Dir string `json:"vault_dir"`
}

func resolved(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// Entry of a new key for the vault in dir, under a random account
func newRef(dir string) *keyRef {
	return &keyRef{Storage: "keychain", Service: keychainService, Account: "vault-" + strings.ReplaceAll(uuid.NewString(), "-", ""), Dir: resolved(dir)}
}

// Whether the entry was made for the vault in dir, not copied or moved from another
func (r *keyRef) owned(dir string) bool {
	return r.Dir == resolved(dir)
}

func (r *keyRef) service() string {
	if r.Service == "" {
		return keychainService
	}
	return r.Service
}

// Entry saved at the path, nil when there is none, an Error when it is unreadable or names no entry
func readRef(path string) (*keyRef, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var ref keyRef
	if err == nil {
		err = json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &ref)
	}
	if err != nil {
		return nil, &Error{fmt.Sprintf("%s is unreadable (%v); see `icebath vault key`", filepath.Base(path), err)}
	}
	if ref.Storage != "keychain" || ref.Account == "" {
		return nil, &Error{filepath.Base(path) + " does not describe a keychain entry; see `icebath vault key`"}
	}
	return &ref, nil
}

// Saves the entry at the path, written at once
func writeRef(path string, ref *keyRef) error {
	data, err := json.MarshalIndent(ref, "", "  ")
	if err != nil {
		return err
	}
	return fsx.Write(path, data, 0o600)
}

// Saves the entry of a key just made, deleting the key again unless the entry landed
func saveNewRef(path string, ref *keyRef) error {
	err := writeRef(path, ref)
	if err == nil {
		return nil
	}
	if saved, rerr := readRef(path); rerr == nil && saved != nil && saved.Account == ref.Account {
		return err
	}
	deleteKey(ref)
	return err
}

// Key of the entry, nil when the entry is gone, KeychainUnavailable when the keychain can't answer
func loadKey(ref *keyRef) ([]byte, error) {
	k, err := osKeyring()
	if err != nil {
		return nil, err
	}
	value, ok, err := k.get(ref.service(), ref.Account)
	if err != nil {
		return nil, &KeychainUnavailable{Msg: refused("the OS keychain did not answer", err)}
	}
	if !ok || value == "" {
		return nil, nil
	}
	return []byte(value), nil
}

// Writes the key and reads it back, a keychain that answers something else being of no use
func storeKey(ref *keyRef, key []byte) error {
	k, err := osKeyring()
	if err != nil {
		return err
	}
	if err := k.set(ref.service(), ref.Account, string(key)); err != nil {
		return &KeychainUnavailable{Msg: refused("the OS keychain refused the key", err)}
	}
	back, _, err := k.get(ref.service(), ref.Account)
	if err != nil {
		return &KeychainUnavailable{Msg: refused("the OS keychain refused the key", err)}
	}
	if back != string(key) {
		return &KeychainUnavailable{Msg: "the OS keychain did not return the key it was given"}
	}
	return nil
}

// Deletes the entry, true once it is gone, an interrupted earlier delete included
func deleteKey(ref *keyRef) bool {
	k, err := osKeyring()
	if err != nil {
		return false
	}
	_ = k.remove(ref.service(), ref.Account)
	_, ok, err := k.get(ref.service(), ref.Account)
	return err == nil && !ok
}

// Removes a file, none there being fine
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
