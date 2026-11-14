package azure

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/internal/vault"
)

const libsecretTrialFailure = `libsecret did not perform properly.
* If you encountered error "Remote error from secret service:
  org.freedesktop.DBus.Error.ServiceUnknown",
  you may need to install gnome-keyring package.
* Headless mode (such as in an ssh session) is not supported.
`

type secretCache struct {
	signal   string
	keychain *vault.Keychain
}

type plainCache struct{ path string }

func cacheAttributes() map[string]string {
	return map[string]string{"MsalClientID": keychainService, "xdg:schema": sharedCacheName}
}

func openCacheStore(ctx context.Context) (cacheStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, ".IdentityService", sharedCacheName)
	keychain := vault.NewKeychain()
	trialErr := secretTrial(ctx, keychain)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	if trialErr != nil {
		slog.Warn(libsecretTrialFailure, "error", trialErr)
		slog.Debug(`Failed to create a LibsecretPersistence: "` + trialErr.Error() + `"`)
		slog.Info("Encryption unavailable. Opting in to plain text.")
		return plainCache{path: path}, nil
	}
	return secretCache{signal: path, keychain: keychain}, nil
}

func secretTrial(ctx context.Context, keychain *vault.Keychain) error {
	attributes := map[string]string{"attr1": "foo", "attr2": "bar", "xdg:schema": "Test Schema"}
	const payload = "Test Data"
	if err := keychain.StoreAttributes(ctx, "", attributes, payload); err != nil {
		return err
	}
	stored, err := keychain.LookupAttributes(ctx, attributes)
	if err != nil {
		return err
	}
	if stored != payload {
		return errors.New("the Secret Service returned other data than it stored")
	}
	return keychain.ClearAttributes(ctx, attributes)
}

func (s secretCache) location() string { return s.signal }

func (s secretCache) modified() (time.Time, bool, error) { return fileModified(s.signal) }

func (s secretCache) load(ctx context.Context) ([]byte, bool, error) {
	secret, err := s.keychain.LookupAttributes(ctx, cacheAttributes())
	if errors.Is(err, vault.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(secret), true, nil
}

func (s secretCache) save(ctx context.Context, data []byte) error {
	if err := s.keychain.StoreAttributes(ctx, keychainAccount, cacheAttributes(), string(data)); err != nil {
		return err
	}
	return touchFile(s.signal)
}

func (p plainCache) location() string { return p.path }

func (p plainCache) modified() (time.Time, bool, error) { return fileModified(p.path) }

func (p plainCache) load(context.Context) ([]byte, bool, error) { return readPlain(p.path) }

func (p plainCache) save(_ context.Context, data []byte) error { return writeInPlace(p.path, data) }
