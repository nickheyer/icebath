package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Whether a turn passes the global emergency stop as a known command or a reply owned by in flight work
func (r *GatewayRunner) hmEstopTurnAllowed(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource) bool {
	if command, ok := event.GetCommand(); ok && command != "" {
		if _, known := ResolveCommand(command); known {
			return true
		}
	}
	key, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return false
	}
	if pending, _ := r.updatePromptPending().Get(key); pending {
		return true
	}
	if r.isSessionRunning(key) {
		return true
	}
	if SlashConfirmPending(key) {
		return true
	}
	return tools.HasBlockingApproval(key)
}

// Emergency stop notice when this turn must be blocked, empty for a dropped heartbeat, false when it may run
func (r *GatewayRunner) hmEstopGate(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, isInternal bool) (string, bool) {
	if isInternal {
		return "", false
	}
	notice := tools.PausedReply(ctx)
	if notice == "" || r.hmEstopTurnAllowed(ctx, event, source) {
		return "", false
	}
	if id := event.HeartbeatSessionID(); id != nil && *id != "" {
		inboundLog().Debug("Heartbeat turn dropped by global emergency stop")
		return "", true
	}
	platform, chat := "unknown", "unknown"
	if source != nil {
		platform = string(source.Platform)
		if source.ChatID != "" {
			chat = source.ChatID
		}
	}
	inboundLog().Info(fmt.Sprintf("Gateway turn paused by global emergency stop (platform=%s chat=%s)", platform, chat))
	return notice, true
}
