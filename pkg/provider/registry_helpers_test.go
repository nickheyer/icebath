package provider

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Restores the provider registry, the home layers and the auth registry mirror when the test ends
func snapshotProviderRegistry(t *testing.T) {
	t.Helper()
	discoverProviders(context.Background())
	regMu.Lock()
	profiles := map[string]*Profile{}
	for k, v := range regProfiles {
		profiles[k] = v
	}
	order := append([]string(nil), regOrder...)
	aliases := map[string]string{}
	for k, v := range regAliases {
		aliases[k] = v
	}
	sources := map[string]string{}
	for k, v := range regSources {
		sources[k] = v
	}
	regMu.Unlock()
	reg := providerRegistry()
	reg.mu.Lock()
	keys := append([]string(nil), reg.keys...)
	rows := map[string]*Profile{}
	for k, v := range reg.rows {
		rows[k] = v
	}
	pending := reg.syncPending
	reg.mu.Unlock()
	pluginMirroredMu.Lock()
	mirrored := map[string]bool{}
	for k, v := range pluginMirroredProviders {
		mirrored[k] = v
	}
	pluginMirroredMu.Unlock()
	t.Cleanup(func() {
		regMu.Lock()
		regProfiles, regOrder, regAliases, regSources, regListCache = profiles, order, aliases, sources, nil
		regMu.Unlock()
		regGeneration.Add(1)
		reg.mu.Lock()
		reg.keys, reg.rows, reg.syncPending = keys, rows, pending
		reg.mu.Unlock()
		pluginMirroredMu.Lock()
		pluginMirroredProviders = mirrored
		pluginMirroredMu.Unlock()
		homeLayersMu.Lock()
		homeLayers = map[string]*homeLayer{}
		homeLayersMu.Unlock()
		homeRowsMu.Lock()
		homeRows = map[[2]*Profile]*Profile{}
		homeRowsMu.Unlock()
		hostMemoMu.Lock()
		hostMemo = map[hostMemoKey]hostMemoEntry{}
		hostMemoMu.Unlock()
		namedCustomMu.Lock()
		namedCustomMem = map[string]*namedCustomMemo{}
		namedCustomMu.Unlock()
	})
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
