package gateway

import "context"

// Runner surface the API server adapter reads, the authorization adapter map and the session model override
type APIServerRunner interface {
	AuthorizationAdapter(ctx context.Context, platform Platform, profile *string) PlatformAdapter
	SessionModelOverrideFor(ctx context.Context, sessionKey string) map[string]any
}

// Gateway run module functions an API server agent is built and reaped with
type APIAgentRuntimeResolver interface {
	ResolveRuntimeAgentKwargs(ctx context.Context) (map[string]any, error)
	ResolveRuntimeAgentKwargsForProvider(ctx context.Context, providerName, targetModel string) (map[string]any, error)
	LoadReasoningConfig(ctx context.Context, model string) map[string]any
	LoadFallbackModel(ctx context.Context) []map[string]any
	ReapGatewayTurnProcesses(ctx context.Context, taskID string, baseline map[string]bool, source string, isStillCurrent func() bool) int
}

// Resolver the gateway runner package installs at init
var APIAgentRuntime APIAgentRuntimeResolver

// Default size and idle bounds of the gateway agent cache, the run module constants
const (
	AgentCacheMaxSize     = 128
	AgentCacheIdleTTLSecs = 3600.0
)
