package gatewayrun

import "context"

// Restores the override a one shot model switch captured, only while the given generation is current
func (r *GatewayRunner) restorePendingOneTurnModelOverride(ctx context.Context, sessionKey string, runGeneration *int) {
	if sessionKey == "" {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("Failed to restore one-turn model override", "error", rec)
		}
	}()
	state := r.peekSessionState(sessionKey)
	if state == nil || len(state.conversation.oneTurnRestore) == 0 {
		return
	}
	if runGeneration != nil && !r.isSessionRunCurrent(sessionKey, *runGeneration) {
		return
	}
	snapshot := state.conversation.oneTurnRestore
	state.conversation.oneTurnRestore = nil
	r.restoreSessionModelOverride(ctx, sessionKey, snapshot)
}
