package gateway

import (
	"sync"

	"github.com/nickheyer/icebath/pkg/plugins"
)

// Provider kinds cmd/icebath wires registries for, the set bundled plugins register into
var pluginProviderKinds = []string{
	"image_gen_provider", "video_gen_provider", "tts_provider", "transcription_provider", "web_search_provider",
	"browser_provider", "browser_page_backend", "terminal_environment_provider", "secret_source",
	"delegation_provider", "login_backend",
}

// Scoped provider registry double that accepts every provider and records it by scope and name
type recordingScopedRegistry struct {
	mu        sync.Mutex
	providers map[[2]string]any
}

func (r *recordingScopedRegistry) Accepts(any) bool { return true }

func (r *recordingScopedRegistry) ProviderName(provider any) string {
	if named, ok := provider.(interface{ Name() string }); ok {
		return named.Name()
	}
	return ""
}

func (r *recordingScopedRegistry) Identity(any) error { return nil }

func (r *recordingScopedRegistry) Snapshot(name, scope string) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.providers[[2]string{scope, name}]
}

func (r *recordingScopedRegistry) Register(provider any, scope string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[[2]string{scope, r.ProviderName(provider)}] = provider
	return true, nil
}

func (r *recordingScopedRegistry) Restore(name string, _, replacement any, scope string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if replacement == nil {
		delete(r.providers, [2]string{scope, name})
	} else {
		r.providers[[2]string{scope, name}] = replacement
	}
	return true
}

// Plugin services of the test binary, one recording registry per provider kind and the gateway's own registries
func pluginServicesDouble() *plugins.Services {
	registries := map[string]plugins.ScopedRegistry{}
	for _, kind := range pluginProviderKinds {
		registries[kind] = &recordingScopedRegistry{providers: map[[2]string]any{}}
	}
	return &plugins.Services{
		ProviderRegistries: registries,
		DashboardAuth:      DashboardAuthRegistryService(),
		Platforms:          PlatformRegistryService(),
		HostObject:         func(string, any) (string, bool) { return "", false },
	}
}

// Installs the plugin services double so plugin discovery under a fresh home loads the bundled plugins
func installPluginServicesDouble() { plugins.InstallServices(pluginServicesDouble()) }
