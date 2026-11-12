package observability

import "context"

// Hooks the owning packages install from an init function, a nil hook behaving as the upstream import failing
var (
	// FailoverReasonsHook lists every provider FailoverReason value, set by pkg/provider
	FailoverReasonsHook func() []string
	// GatewayAdapterPlatformsHook lists the gateway Platform enum members, set by pkg/gateway
	GatewayAdapterPlatformsHook func() []string
	// PluginPlatformNamesHook lists the platform registry's plugin entry names, set by pkg/gateway
	PluginPlatformNamesHook func(ctx context.Context) []string
	// BundledPluginKeysHook lists the registry keys of the compiled in plugins, set by pkg/plugins
	BundledPluginKeysHook func() []string
	// BundledMemoryProvidersHook lists the bundled memory provider names, set by pkg/memory
	BundledMemoryProvidersHook func() []string
	// CatalogPlatformOwnerHook returns the installer recorded catalog name of the plugin registering a platform, set by pkg/gateway
	CatalogPlatformOwnerHook func(homeDir, platform string) (any, bool)
	// ResolveCommandHook returns the canonical registry name of a slash command or alias, set by the command registry owner
	ResolveCommandHook func(name string) (string, bool)
	// ResolveSkillCommandKeyHook reports whether a name resolves to a skill slash command of the bound home, set by pkg/skills
	ResolveSkillCommandKeyHook func(ctx context.Context, name string) (bool, error)
	// PluginCommandHandlerHook reports whether a plugin registered a handler for a command, set by pkg/plugins
	PluginCommandHandlerHook func(ctx context.Context, name string) bool
	// ModelsDevProviderHook reports whether the local models.dev cache knows a provider id, set by pkg/provider
	ModelsDevProviderHook func(name string) bool
)

// Every FailoverReason value the provider classifier can return
func failoverReasonValues() ValueSet {
	if FailoverReasonsHook == nil {
		return ValueSet{}
	}
	return Set(FailoverReasonsHook()...)
}

// The gateway's own adapter vocabulary
func gatewayAdapterPlatforms() ValueSet {
	return Set(GatewayAdapterPlatformsHook()...)
}

// Bundled memory provider names
func bundledMemoryProviders() ValueSet {
	if BundledMemoryProvidersHook == nil {
		return ValueSet{}
	}
	return Set(BundledMemoryProvidersHook()...)
}

// SkillSourceIDAliasesHook returns the skills hub source alias table, set by pkg/skills
var SkillSourceIDAliasesHook func() map[string]string
