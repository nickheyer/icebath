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
	var snapshot map[string]any
	r.withPeekedSessionState(sessionKey, func(s *SessionState) {
		if len(s.conversation.oneTurnRestore) == 0 || (runGeneration != nil && s.persistent.runGeneration != *runGeneration) {
			return
		}
		snapshot = s.conversation.oneTurnRestore
		s.conversation.oneTurnRestore = nil
	})
	if snapshot == nil {
		return
	}
	r.restoreSessionModelOverride(ctx, sessionKey, snapshot)
}
