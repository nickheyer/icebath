package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Session, platform, task and turn of the Relay turn the current work inherits, false without one
func relayCurrentTurnIdentity(ctx context.Context) (sessionID, platform, taskID, turnID string, ok bool) {
	turn := tools.CurrentRelayTurn(ctx)
	if turn == nil {
		return "", "", "", "", false
	}
	if turn.Lease != nil {
		sessionID, platform = turn.Lease.SessionID, turn.Lease.Platform
	}
	return sessionID, platform, turn.TaskID, turn.TurnID, true
}
