package provider

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Restores the provider registry, home layers, auth registry mirror and picker catalog tables when the test ends
func snapshotProviderRegistry(t *testing.T) {
	t.Helper()
	t.Cleanup(SnapshotProviderRegistryForTests())
}

// Registers a profile process wide for one test, the registry restored afterwards
func registerTestProfile(t *testing.T, p Profile) {
	t.Helper()
	snapshotProviderRegistry(t)
	RegisterProvider(p)
}

// Plugin loader double returning profiles per plugin dir name, restoring the real loader afterwards
func fakeExternalProviderPlugins(t *testing.T, byDir map[string]func(homeKey string) ([]Profile, error)) *[]string {
	t.Helper()
	calls := &[]string{}
	prev := loadExternalProviderPluginFn
	loadExternalProviderPluginFn = func(ctx context.Context, dir, source, homeKey string) ([]Profile, error) {
		name := filepath.Base(dir)
		*calls = append(*calls, name)
		if fn, ok := byDir[name]; ok {
			return fn(homeKey)
		}
		return nil, nil
	}
	t.Cleanup(func() { loadExternalProviderPluginFn = prev })
	return calls
}

// Forces the next plugin dir check of every layer to run regardless of the stamp TTL
func expireHomeLayerChecks() {
	homeLayersMu.Lock()
	defer homeLayersMu.Unlock()
	for _, l := range homeLayers {
		l.mu.Lock()
		l.stampCheckedAt = time.Time{}
		l.misses = map[string]time.Time{}
		l.mu.Unlock()
	}
}

// Registers an oauth external profile carrying the hook bundle, the registry restored when the test ends
func registerHookedProfile(t *testing.T, name string, hooks ProviderProfileHooks) {
	t.Helper()
	registerTestProfile(t, Profile{Name: name, AuthType: AuthTypeOAuthExternal, BaseURL: "https://example.invalid/v1",
		RefreshCredential: hooks.RefreshCredential, SeedPoolCredentialsHook: hooks.SeedPoolCredentials,
		RemoveCredentialSourceHook: hooks.RemoveCredentialSource, AuthHandler: hooks.AuthHandler})
}
