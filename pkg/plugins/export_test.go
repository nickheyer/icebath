package plugins

import "context"

// Leases the deferred platform loader of a manifest the way discovery does, for the external test package
func RegisterDeferredPlatformOf(ctx context.Context, m *Manager, mf *Manifest) error {
	return m.registerDeferredPlatform(ctx, mf)
}

// Runs fn holding the discovery lock of a manager, handing it the unload that runs inside that lock
func UnderDiscoveryLock(m *Manager, fn func(unload func(plugin any) bool)) {
	m.discoveryMu.Lock()
	defer m.discoveryMu.Unlock()
	fn(m.unloadScoped)
}

// Whether a manager lists a platform name as plugin owned
func OwnsPlatformName(m *Manager, name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pluginPlatformNames[name]
}

// Bundled manifest of a compiled-in plugin key, nil when no such plugin is registered
func BundledManifestOf(key string) *Manifest {
	for _, mf := range bundledManifests() {
		if mf.Key == key {
			return mf
		}
	}
	return nil
}

// Empties the bundled table for the external test main
func DropImportedBundledPlugins() { dropImportedBundledPlugins() }

// Installs the tools env hooks the gateway and profile ports install in production, for the external test package
func InstallHostEnvHooksForTests() { installHostEnvHooks() }

// Services the process installed, for an external case restoring them
func InstalledServicesForTests() *Services { return installedServices() }

// Host of a home with its base context built the way discovery builds it, for the external test package
func NewPluginHostForTests(h string) (*PluginHost, error) { return newPluginHost(NewManager(h)) }
