package plugins

import "sync"

var (
	providerRegistriesMu sync.Mutex
	// Provider kinds the linked tiers register, each with the registry plugins of that kind load into
	tierProviderRegistries = map[string]func() ScopedRegistry{}
)

// Registers the scoped registry plugins of a provider kind load into, called at init by its owner
func RegisterProviderRegistry(kind string, registry func() ScopedRegistry) {
	providerRegistriesMu.Lock()
	tierProviderRegistries[kind] = registry
	providerRegistriesMu.Unlock()
}

// Scoped registries of every provider kind the linked packages registered
func RegisteredProviderRegistries() map[string]ScopedRegistry {
	providerRegistriesMu.Lock()
	defer providerRegistriesMu.Unlock()
	out := make(map[string]ScopedRegistry, len(tierProviderRegistries))
	for kind, registry := range tierProviderRegistries {
		out[kind] = registry()
	}
	return out
}
