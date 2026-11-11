package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Stored session a local receipt names, empty for a session with no readable local receipt
func localCloseTarget(ctx context.Context, a *SessionAuthority, sessionID string) string {
	receipt, err := a.DB.LocalReceipt(ctx, sessionID)
	if err != nil || receipt == nil {
		return ""
	}
	entry, ok := receipt.GetOr("entry", nil).(*pyjson.Object)
	if !ok {
		return ""
	}
	target, _ := entry.GetOr("session_id", "").(string)
	return target
}

// A live feed row main would list as resumable, closed idle and with no viewer left
func closedAndUnviewed(ctx context.Context, a *SessionAuthority, sessionID string, handle SessionHandle) (bool, error) {
	live, ok := a.live(sessionID)
	if !ok || len(live.Subscribers) > 0 || handle.ExecutionState != "idle" {
		return false, nil
	}
	if liveDetached(live) || isHistoryMount(live) {
		return true, nil
	}
	var endedAt any
	if target := localCloseTarget(ctx, a, sessionID); target != "" {
		row, err := a.DB.GetSessionListRow(ctx, target)
		if err != nil {
			return false, err
		}
		if row != nil {
			endedAt = row.GetOr("ended_at", nil)
		}
	}
	return closedIdleUnviewed(live, handle.ExecutionState, endedAt), nil
}
