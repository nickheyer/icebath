package tuiserver

import (
	"github.com/nickheyer/icebath/pkg/agent"
)

// Parent agent surface a side agent copies its route, toolsets and store from
type sideAgentParent interface {
	BaseURL() string
	APIKey() string
	Provider() string
	APIMode() string
	ACPCommand() string
	ACPArgs() []string
	EphemeralSystemPrompt() string
	ProviderRouting() agent.ProviderRouting
	Model() string
	EnabledToolsets() []string
	DisabledToolsets() []string
	ReasoningConfig() map[string]any
	ServiceTier() string
	RequestOverrides() map[string]any
	SessionStore() agent.SessionStore
	FallbackChain() []map[string]any
}

var _ sideAgentParent = (*agent.AIAgent)(nil)
