package gatewayrun

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
)

var (
	_ gateway.RunnerModelContract = (*GatewayRunner)(nil)
	_ gateway.PeekedSessionState  = (*SessionState)(nil)
)

// Channel overrides entry of the chat a source arrived on, nil when none applies
func (r *GatewayRunner) ChannelOverrideFor(source *gateway.SessionSource) *gateway.ChannelOverride {
	return r.channelOverrideFor(source)
}

// In memory model override of a session, nil when it holds none
func (r *GatewayRunner) SessionModelOverride(sessionKey string) map[string]any {
	return r.sessionModelOverride(sessionKey)
}

// Restores a persisted model override after a restart unless one is already in memory
func (r *GatewayRunner) RehydrateSessionModelOverride(ctx context.Context, sessionKey string) {
	r.rehydrateSessionModelOverride(ctx, sessionKey)
}

// Reasoning of a session, its override, a local policy level, else the per model or global config
func (r *GatewayRunner) ResolveSessionReasoningConfig(ctx context.Context, source *gateway.SessionSource, sessionKey, model string) (map[string]any, error) {
	return r.resolveSessionReasoningConfig(ctx, source, sessionKey, model)
}

// Sets the session scoped reasoning override, a nil config clearing it
func (r *GatewayRunner) SetSessionReasoningOverride(sessionKey string, reasoningConfig map[string]any) {
	r.setSessionReasoningOverride(sessionKey, reasoningConfig)
}

// Model and runtime of a session, its model override first, then channel overrides, then the config
func (r *GatewayRunner) ResolveSessionAgentRuntime(ctx context.Context, source *gateway.SessionSource, sessionKey string, userConfig map[string]any, fallback bool) (string, map[string]any, error) {
	return r.resolveSessionAgentRuntime(ctx, source, sessionKey, userConfig, fallback)
}

// Drops the session model override so the next turn runs the channel or config route, the reply returned
func (r *GatewayRunner) HandleModelReset(ctx context.Context, event *gateway.MessageEvent, sw *gateway.ModelSwitchContext) (*string, error) {
	return r.handleModelReset(ctx, event, sw)
}

// Resolves a model switch to its result, or a nil result with the error reply text
func (r *GatewayRunner) PerformModelSwitch(ctx context.Context, sw *gateway.ModelSwitchContext, rawInput, explicitProvider string, source *gateway.SessionSource) (*provider.ModelSwitchResult, string, error) {
	return r.performModelSwitch(ctx, sw, rawInput, explicitProvider, source)
}

// Persists a committed switch as the session override, the global write warning when one failed
func (r *GatewayRunner) RecordModelSwitch(ctx context.Context, result *provider.ModelSwitchResult, sw *gateway.ModelSwitchContext, source *gateway.SessionSource, oneTurn, picker bool) string {
	return r.recordModelSwitch(ctx, result, sw, source, oneTurn, picker)
}

// Runner wide lock over a model command's read, resolve and commit
func (r *GatewayRunner) ModelSwitchLock() *sync.Mutex {
	return r.modelSwitchLock()
}
