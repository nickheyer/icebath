package gatewayrun

import (
	"context"
	"fmt"
	"math/big"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Configured active chat session cap, nil when disabled or unreadable
func (r *GatewayRunner) getMaxConcurrentSessions() (n *big.Int) {
	defer func() {
		if rec := recover(); rec != nil {
			n = nil
		}
	}()
	return activesessions.ResolveMaxConcurrentSessions(r.config)
}

// User facing rejection when starting a new session exceeds the cap, false when it fits
func (r *GatewayRunner) activeSessionLimitMessage(sessionKey string) (string, bool) {
	maxSessions := r.getMaxConcurrentSessions()
	if maxSessions == nil || r.isSessionRunning(sessionKey) {
		return "", false
	}
	active := r.runningAgentCount()
	if big.NewInt(int64(active)).Cmp(maxSessions) < 0 {
		return "", false
	}
	return activesessions.ActiveSessionLimitMessage(active, maxSessions, nil), true
}

// Claims a cross process active session slot for a new gateway turn, nil lease and refusal when already running
func (r *GatewayRunner) claimActiveSessionSlot(ctx context.Context, sessionKey string, source *gateway.SessionSource) (*activesessions.Lease, *activesessions.Refusal) {
	if r.isSessionRunning(sessionKey) {
		return nil, nil
	}
	if message, refused := r.activeSessionLimitMessage(sessionKey); refused {
		return nil, &activesessions.Refusal{Message: message}
	}
	platform, chatID, userID := "gateway", "", ""
	if source != nil {
		if source.Platform != "" {
			platform = string(source.Platform)
		}
		chatID = source.ChatID
		if source.UserID != nil {
			userID = *source.UserID
		}
	}
	lease, refusal, err := activesessions.TryAcquire(ctx, sessionKey, "gateway:"+platform, r.config, map[string]any{
		"platform": platform, "chat_id": chatID, "user_id": userID, "live_session_id": sessionKey,
	}, "", false)
	if err != nil {
		runLog().Warn(fmt.Sprintf("Failed to claim active session slot: %v", err))
		return nil, nil
	}
	return lease, refusal
}
