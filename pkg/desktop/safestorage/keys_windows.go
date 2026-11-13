//go:build windows

package safestorage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	dpapiKeyPrefix = "DPAPI"
	gcmNonceLength = 12
	windowsKeySize = 32
)

type platformKeys struct {
	localState string
	loaded     bool
	key        []byte
}

func newPlatformKeys(o Options) *platformKeys {
	return &platformKeys{localState: filepath.Join(o.UserData, "Local State")}
}

// Reads the DPAPI wrapped AES key of Local State, writing a fresh one when it is absent or unreadable
func (k *platformKeys) load() {
	if k.loaded {
		return
	}
	k.loaded = true
	state := map[string]any{}
	if raw, err := os.ReadFile(k.localState); err == nil {
		if json.Unmarshal(raw, &state) != nil || state == nil {
			state = map[string]any{}
		}
	}
	osCrypt, _ := state["os_crypt"].(map[string]any)
	if encoded, ok := osCrypt["encrypted_key"].(string); ok {
		if wrapped, err := base64.StdEncoding.DecodeString(encoded); err == nil && bytes.HasPrefix(wrapped, []byte(dpapiKeyPrefix)) {
			if key, err := dpapiUnprotect(wrapped[len(dpapiKeyPrefix):]); err == nil {
				k.key = key
				return
			}
		}
	}
	key := make([]byte, windowsKeySize)
	if _, err := rand.Read(key); err != nil {
		return
	}
	wrapped, err := dpapiProtect(key)
	if err != nil {
		return
	}
	if osCrypt == nil {
		osCrypt = map[string]any{}
	}
	osCrypt["encrypted_key"] = base64.StdEncoding.EncodeToString(append([]byte(dpapiKeyPrefix), wrapped...))
	state["os_crypt"] = osCrypt
	if writeLocalState(k.localState, state) != nil {
		return
	}
	k.key = key
}

func (k *platformKeys) available(bool) bool {
	k.load()
	return k.key != nil
}

func (k *platformKeys) encrypt(plaintext []byte) ([]byte, error) {
	aead, err := k.aead()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcmNonceLength)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(append([]byte("v10"), nonce...), nonce, plaintext, nil), nil
}

func (k *platformKeys) decrypt(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, []byte("v10")) {
		return dpapiUnprotect(data)
	}
	data = data[3:]
	if len(data) < gcmNonceLength {
		return nil, errors.New("ciphertext shorter than its nonce")
	}
	aead, err := k.aead()
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, data[:gcmNonceLength], data[gcmNonceLength:], nil)
}

func (k *platformKeys) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(k.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Writes Local State through a sibling temp file so a crash never leaves it half written
func writeLocalState(path string, state map[string]any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(state); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, bytes.TrimSuffix(buf.Bytes(), []byte("\n")), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Encrypts for the current user with DPAPI and an empty description, as Chromium wraps its key
func dpapiProtect(data []byte) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	name, _ := windows.UTF16PtrFromString("")
	if err := windows.CryptProtectData(&in, name, nil, 0, nil, 0, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte{}, unsafe.Slice(out.Data, out.Size)...), nil
}

// Decrypts a current user DPAPI blob
func dpapiUnprotect(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty DPAPI blob")
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte{}, unsafe.Slice(out.Data, out.Size)...), nil
}
