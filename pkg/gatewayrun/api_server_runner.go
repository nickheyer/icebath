package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Live adapter of a profile whose intake policy gates authorization, the API server platform callback target
func (r *GatewayRunner) AuthorizationAdapter(ctx context.Context, platform gateway.Platform, profile *string) gateway.PlatformAdapter {
	return r.authorizationAdapter(ctx, platform, profile)
}

// Session model override of a key after rehydrating a persisted one, a copy the caller may keep
func (r *GatewayRunner) SessionModelOverrideFor(ctx context.Context, sessionKey string) map[string]any {
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug("api_server failed to rehydrate session /model override for "+sessionKey, "error", rec)
			}
		}()
		r.rehydrateSessionModelOverride(ctx, sessionKey)
	}()
	override := r.sessionModelOverride(sessionKey)
	if override == nil {
		return nil
	}
	out := make(map[string]any, len(override))
	for k, v := range override {
		out[k] = v
	}
	return out
}

// Gateway run module functions the API server builds and reaps agents with
type apiAgentRuntime struct{}

func (apiAgentRuntime) ResolveRuntimeAgentKwargs(ctx context.Context) (map[string]any, error) {
	return resolveRuntimeAgentKwargs(ctx)
}

func (apiAgentRuntime) ResolveRuntimeAgentKwargsForProvider(ctx context.Context, providerName, targetModel string) (map[string]any, error) {
	return resolveRuntimeAgentKwargsForProvider(ctx, providerName, targetModel)
}

func (apiAgentRuntime) LoadReasoningConfig(ctx context.Context, model string) map[string]any {
	return loadReasoningConfig(ctx, model)
}

func (apiAgentRuntime) LoadFallbackModel(ctx context.Context) []map[string]any {
	return loadFallbackModel(ctx)
}

func (apiAgentRuntime) ReapGatewayTurnProcesses(ctx context.Context, taskID string, baseline map[string]bool, source string, isStillCurrent func() bool) int {
	return reapGatewayTurnProcesses(ctx, taskID, baseline, source, isStillCurrent)
}

func init() { gateway.APIAgentRuntime = apiAgentRuntime{} }
