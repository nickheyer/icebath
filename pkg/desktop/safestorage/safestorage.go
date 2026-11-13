package safestorage

import (
	"errors"
	"sync"
)

// Options name the Electron app whose OS key store entries are read and written
type Options struct {
	// Electron app name, the keychain service and keyring application of its key
	AppName string
	// Electron userData directory, home of Local State on Windows
	UserData string
	// Linux --password-store backend, empty for the desktop environment default
	PasswordStore string
}

// Storage is Electron safeStorage over the OS key store, reading and writing the ciphertext Electron writes
type Storage struct {
	mu    sync.Mutex
	keys  *platformKeys
	plain bool
}

var (
	errEncryptUnavailable = errors.New("Error while encrypting the text provided to safeStorage.encryptString. Encryption is not available.")
	errEncrypt            = errors.New("Error while encrypting the text provided to safeStorage.encryptString.")
	errDecryptUnavailable = errors.New("Error while decrypting the ciphertext provided to safeStorage.decryptString. Decryption is not available.")
	errDecrypt            = errors.New("Error while decrypting the ciphertext provided to safeStorage.decryptString.")
)

// New opens the key store of the platform lazily, nothing touches it before the first call
func New(o Options) *Storage {
	return &Storage{keys: newPlatformKeys(o)}
}

// SetUsePlainTextEncryption lets the Linux basic backend encrypt with the fixed v10 key
func (s *Storage) SetUsePlainTextEncryption(use bool) {
	s.mu.Lock()
	s.plain = use
	s.mu.Unlock()
}

// IsEncryptionAvailable reports whether the OS key store yields a key, touching it on first use
func (s *Storage) IsEncryptionAvailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys.available(s.plain)
}

// EncryptString seals plaintext in the versioned format Electron writes, empty for empty input
func (s *Storage) EncryptString(plaintext string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.keys.available(s.plain) {
		return nil, errEncryptUnavailable
	}
	if plaintext == "" {
		return []byte{}, nil
	}
	out, err := s.keys.encrypt([]byte(plaintext))
	if err != nil {
		return nil, errEncrypt
	}
	return out, nil
}

// DecryptString opens ciphertext Electron or EncryptString wrote, unversioned bytes pass through
func (s *Storage) DecryptString(ciphertext []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.keys.available(s.plain) {
		return "", errDecryptUnavailable
	}
	if len(ciphertext) == 0 {
		return "", nil
	}
	out, err := s.keys.decrypt(ciphertext)
	if err != nil {
		return "", errDecrypt
	}
	return string(out), nil
}
