package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Retires the delete targets and removes their rows, terminal rows surviving as digest tombstones
func deleteInTransaction(d *SessionDB, ctx context.Context, conn SQLiteConn, sessionID string, payload *pyjson.Object) ([]string, *pyjson.Object, error) {
	targets, err := DeleteTargets(ctx, conn, sessionID)
	if err != nil {
		return nil, nil, err
	}
	if err := d.RequireIdle(ctx, conn, targets); err != nil {
		return nil, nil, err
	}
	if err := retireSessions(ctx, conn, targets, nil); err != nil {
		return nil, nil, err
	}
	for _, sid := range targets {
		if err := bumpConversationGeneration(ctx, conn, sid, "session_reset"); err != nil {
			return nil, nil, err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET parent_session_id=NULL, runtime_revision=runtime_revision+1 WHERE parent_session_id=?", sid); err != nil {
			return nil, nil, err
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM messages WHERE session_id=?", sid); err != nil {
			return nil, nil, err
		}
	}
	for _, sid := range targets {
		if _, err := conn.ExecContext(ctx, "DELETE FROM sessions WHERE id=?", sid); err != nil {
			return nil, nil, err
		}
	}
	if err := deleteUnreferencedSystemPrompts(ctx, conn); err != nil {
		return nil, nil, err
	}
	return []string{}, pyjson.ObjectOf("deleted_ids", anyStrings(targets)), nil
}
