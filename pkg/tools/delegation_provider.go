package tools

// Task fields core owns that a delegation provider may never declare as its own task parameter
var ReservedTaskParameters = map[string]bool{
	"goal": true, "context": true, "index": true, "output_schema": true, "images": true, "group": true, "role": true,
	"toolsets": true, "acp_command": true, "acp_args": true, "provider": true, "model": true, "base_url": true,
	"api_key": true, "api_mode": true,
}

// Keys a provider route may carry, every other key of its answer is dropped
var RouteKeys = map[string]bool{
	"model": true, "provider": true, "base_url": true, "api_key": true, "api_mode": true, "request_overrides": true,
	"reasoning_effort": true, "service_tier": true, "fallback_providers": true, "profile": true, "label": true,
}

// Refusal of one task whose message becomes the delegate_task tool error the model reads
type DelegationRouteError struct{ Message string }

func (e *DelegationRouteError) Error() string { return e.Message }

// Pluggable strategy turning one delegate_task task into a child route, selected by delegation.backend
type DelegationProvider interface {
	Name() string
	DisplayName() string
	GetSetupSchema() map[string]any
	Settings(config map[string]any) map[string]any
	Summary() string
	IsAvailable() bool
	TaskParameters(config map[string]any) map[string]map[string]any
	Description(config map[string]any) string
	RouteTask(task, config map[string]any) (map[string]any, error)
}

// Domain method defaults a concrete provider embeds with Self bound to itself
type DelegationProviderDefaults struct{ Self DelegationProvider }

// Backend block under delegation.backends keyed by the provider name, empty when unset or not a mapping
func (d DelegationProviderDefaults) Settings(config map[string]any) map[string]any {
	backends, _ := config["backends"].(map[string]any)
	if own, ok := backends[d.Self.Name()].(map[string]any); ok {
		return own
	}
	return map[string]any{}
}

// Empty picker line
func (DelegationProviderDefaults) Summary() string { return "" }

// Always available
func (DelegationProviderDefaults) IsAvailable() bool { return true }

// No extra task parameters
func (DelegationProviderDefaults) TaskParameters(map[string]any) map[string]map[string]any {
	return map[string]map[string]any{}
}

// No description addendum
func (DelegationProviderDefaults) Description(map[string]any) string { return "" }
