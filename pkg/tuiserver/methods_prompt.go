package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
)

// Reopens a finalized session row before the first real turn writes to it, a failed read never blocking the send
func reopenIfFinalized(ctx context.Context, db *state.DB, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	row, err := db.GetSession(ctx, sessionID)
	if err != nil {
		logger().Debug("finalized-session reopen check failed for "+sessionID, "error", err)
		return nil
	}
	if row == nil {
		return nil
	}
	if endedAt, present := row.Vals["ended_at"]; present && endedAt != nil {
		return db.ReopenSession(ctx, sessionID)
	}
	return nil
}
