package tools

import "context"

// Web extract provider the registry resolves for env, nil when none is available
func ActiveExtractProvider(env *Env) WebProvider { return activeExtractProvider(env) }

// True when a web provider is keyed available or keyless capable without raising
func WebProviderIsReady(env *Env, p WebProvider) bool { return providerIsReady(env, p) }

// Discovers plugins so plugin registered web providers exist before a readiness read
func EnsureWebPluginsLoaded(ctx context.Context, env *Env) { ensureWebPluginsLoaded(ctx, env) }

// Plugin terminal backend registered under a name in scope then globally, nil when absent
func PluginTerminalBackend(ctx context.Context, name, scope string) BackendProvider {
	return terminalEnvRegistry.GetProvider(ctx, name, scope)
}
