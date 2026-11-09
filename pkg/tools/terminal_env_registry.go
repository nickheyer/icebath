package tools

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/providerregistry"
)

// Untyped view of the plugin terminal backend registry the plugin context register_terminal_environment_provider drives
func TerminalEnvRegistryScoped() providerregistry.ScopedRegistry { return terminalEnvRegistry.Scoped() }

// Lower cased names of every plugin backend visible in scope, sorted
func PluginBackendNames(ctx context.Context, scope string) []string {
	names := []string{}
	for _, p := range terminalEnvRegistry.ListProviders(ctx, scope) {
		names = append(names, providerregistry.LowerKey(p.Name()))
	}
	return names
}

// Classification attribute of the plugin backend of a name, def for an unknown backend or a panicking attribute
func TerminalProviderFlag(ctx context.Context, name, attr string, def any, scope string) (value any) {
	provider := terminalEnvRegistry.GetProvider(ctx, name, scope)
	if provider == nil {
		return def
	}
	defer func() {
		if r := recover(); r != nil {
			terminalEnvRegistryLog.Debug(fmt.Sprintf("Terminal environment provider '%s' attribute '%s' raised", name, attr), "panic", providerregistry.PanicText(r))
			value = def
		}
	}()
	return backendAttribute(provider, attr, def)
}

// Python attribute read of a backend provider by its hermes attribute name, def for a name the contract lacks
func backendAttribute(p BackendProvider, attr string, def any) any {
	switch attr {
	case "name":
		return p.Name()
	case "display_name":
		return p.DisplayName()
	case "description":
		return p.Description()
	case "env_description":
		return p.EnvDescription()
	case "is_remote":
		return p.IsRemote()
	case "is_container":
		return p.IsContainer()
	case "skip_container_guards":
		return p.SkipContainerGuards()
	case "cache_path_base":
		if base := p.CachePathBase(); base != "" {
			return base
		}
		return nil
	case "default_cwd":
		if cwd := p.DefaultCwd(); cwd != "" {
			return cwd
		}
		return nil
	case "strip_env_keys":
		keys := map[string]bool{}
		for _, k := range p.StripEnvKeys() {
			keys[k] = true
		}
		return keys
	case "session_isolated_when_nonpersistent":
		return p.SessionIsolatedWhenNonpersistent()
	case "supports_streams":
		return p.SupportsStreams()
	}
	return def
}

// Union of every registered plugin backend's strip_env_keys across every scope, a panicking provider contributing nothing
func PluginStripEnvKeys() map[string]bool {
	keys := map[string]bool{}
	for _, provider := range terminalEnvRegistry.AllProviders() {
		for _, k := range safeStripEnvKeys(provider) {
			keys[k] = true
		}
	}
	return keys
}

// Strip env keys of one provider, empty when reading them panics
func safeStripEnvKeys(p BackendProvider) (keys []string) {
	defer func() {
		if r := recover(); r != nil {
			terminalEnvRegistryLog.Debug("Terminal environment provider strip_env_keys raised", "panic", providerregistry.PanicText(r))
			keys = nil
		}
	}()
	return p.StripEnvKeys()
}

// Generation pair of the plugin terminal backend registry for a scope
func TerminalEnvRegistryGeneration(ctx context.Context, scope string) providerregistry.Generation {
	return terminalEnvRegistry.RegistryGeneration(ctx, scope)
}

// Clears every plugin terminal backend registration, for tests only
func ResetTerminalEnvRegistryForTests() { terminalEnvRegistry.ResetForTests() }
