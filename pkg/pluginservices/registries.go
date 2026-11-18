package pluginservices

import (
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Registry of every provider kind a plugin registers into, the core kinds and those the linked tiers registered
func ProviderRegistries() map[string]plugins.ScopedRegistry {
	out := map[string]plugins.ScopedRegistry{
		"web_search_provider":           agent.WebSearchScopedRegistry(),
		"terminal_environment_provider": agent.TerminalEnvScopedRegistry(),
		"secret_source":                 provider.SecretSourceScopedRegistry(),
		"delegation_provider":           agent.DelegationScopedRegistry(),
	}
	for kind, registry := range plugins.RegisteredProviderRegistries() {
		out[kind] = registry
	}
	return out
}
