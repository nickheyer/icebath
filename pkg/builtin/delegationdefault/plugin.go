package delegationdefault

import (
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func init() { plugins.RegisterBundled(Plugin{}) }

// Compiled-in default delegation strategy plugin
type Plugin struct{}

// Manifest of the delegation/default plugin from its plugin.yaml
func (Plugin) Manifest() plugins.Manifest {
	return plugins.Manifest{
		Name:        "default",
		Key:         "delegation/default",
		Version:     "1.0.0",
		Description: "Default delegation strategy: every delegate_task subagent follows the delegation section of config.yaml (delegation.provider / model / base_url ...), else inherits the parent's model and credentials.",
		Author:      "NousResearch",
		Kind:        "backend",
	}
}

// Registers the default delegation provider
func (Plugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterDelegationProvider(NewDefaultDelegationProvider())
	return err
}

// Delegation provider routing every task through the delegation config section
type DefaultDelegationProvider struct {
	provider.ProviderDefaults
	tools.DelegationProviderDefaults
}

// Provider with both default method sets bound to itself
func NewDefaultDelegationProvider() *DefaultDelegationProvider {
	p := &DefaultDelegationProvider{}
	p.ProviderDefaults.Self = p
	p.DelegationProviderDefaults.Self = p
	return p
}

// Delegation backend config value default
func (*DefaultDelegationProvider) Name() string { return "default" }

// Picker label Default
func (*DefaultDelegationProvider) DisplayName() string { return "Default" }

// Picker line describing the delegation section route with the parent model fallback
func (*DefaultDelegationProvider) Summary() string {
	return "Every subagent uses delegation.model / delegation.provider, else the parent's model."
}

// Delegation section as the route minus service_tier, which that section never carries as a speed tier
func (*DefaultDelegationProvider) RouteTask(_, config map[string]any) (map[string]any, error) {
	route := make(map[string]any, len(config))
	for k, v := range config {
		if k != "service_tier" {
			route[k] = v
		}
	}
	return route, nil
}

var _ tools.DelegationProvider = (*DefaultDelegationProvider)(nil)
