package state

import "context"

// Runs an end stamp update and bumps the conversation generation, the writer entry of wrappers outside the package
func (d *SessionDB) EndAndBump(ctx context.Context, conn SQLiteConn, query string, params []any, sessionID, reason string) (int64, error) {
	return d.endAndBump(ctx, conn, query, params, sessionID, reason)
}
