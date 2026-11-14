package azure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/internal/vault"
)

type keychainCache struct {
	signal   string
	keychain *vault.Keychain
}

func openCacheStore(context.Context) (cacheStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, ".IdentityService", sharedCacheName)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	return keychainCache{signal: path, keychain: vault.NewKeychain()}, nil
}

func (k keychainCache) location() string { return k.signal }

func (k keychainCache) modified() (time.Time, bool, error) { return fileModified(k.signal) }

func (k keychainCache) load(ctx context.Context) ([]byte, bool, error) {
	secret, err := k.keychain.GetForeign(ctx, keychainService, keychainAccount)
	if errors.Is(err, vault.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(secret), true, nil
}

func (k keychainCache) save(ctx context.Context, data []byte) error {
	if err := k.keychain.UpdateForeign(ctx, keychainService, keychainAccount, data); err != nil {
		return err
	}
	return touchFile(k.signal)
}
