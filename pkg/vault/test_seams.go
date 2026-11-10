package vault

import (
	"os"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

// MemoryKeyring is an in memory OS keychain stand in for tests of the packages that drive the vault
type MemoryKeyring struct {
	mu      sync.Mutex
	entries map[[2]string]string
}

func (*MemoryKeyring) keyringModule() string { return "tests.agent.test_vault_key_storage" }
func (*MemoryKeyring) keyringClass() string  { return "MemoryKeyring" }

func (k *MemoryKeyring) GetPassword(service, account string) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.entries[[2]string{service, account}]
	return v, ok, nil
}

func (k *MemoryKeyring) SetPassword(service, account, password string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.entries[[2]string{service, account}] = password
	return nil
}

func (k *MemoryKeyring) DeletePassword(service, account string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.entries[[2]string{service, account}]; !ok {
		return &keyringError{"PasswordDeleteError", "no such entry"}
	}
	delete(k.entries, [2]string{service, account})
	return nil
}

// Count is how many entries the keyring holds
func (k *MemoryKeyring) Count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.entries)
}

// UseMemoryKeyring installs an in memory keyring as the host keychain, the returned func restores the native one
func UseMemoryKeyring() (*MemoryKeyring, func()) {
	k := &MemoryKeyring{entries: map[[2]string]string{}}
	prev := keyringGetKeyring
	keyringGetKeyring = func() (keyringBackend, error) { return k, nil }
	return k, func() { keyringGetKeyring = prev }
}

// EncryptVaultBody writes plaintext as this store's encrypted vault file, the way a test corrupts a store
func (s *VaultStore) EncryptVaultBody(plaintext []byte) error {
	f, err := s.fernet()
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	return pyfs.AtomicWriteBytes(s.vaultPath, f.Encrypt(plaintext), "", &mode, true)
}

// RequiredFields lists the fields a payment or address item must carry
func RequiredFields(kind string) []string { return append([]string{}, vaultRequiredFields[kind]...) }
