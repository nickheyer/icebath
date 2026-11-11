package state

import "context"

// Session row of the runtime ledger, refused not found when absent
func RuntimeSession(ctx context.Context, conn SQLiteConn, sessionID string) (map[string]any, error) {
	return runtimeSession(ctx, conn, sessionID)
}

// Hex sha256 of an adoption secret, refused like any other invalid runtime text
func SecretDigest(secret string) (string, error) { return secretDigest(secret) }

// Retires an execution's children and shrinks its receipt bearing history to the retired marker
func CompactTerminalReceipts(ctx context.Context, conn SQLiteConn, executionID string) error {
	return compactTerminalReceipts(ctx, conn, executionID)
}

// One row of a query as a column mapping, nil when the query returns none
func ConnRow(ctx context.Context, conn SQLiteConn, query string, args ...any) (map[string]any, error) {
	return connRow(ctx, conn, query, args...)
}

// Keep existing constructor upsert on the caller's connection, refusing to recreate a retired session
func (d *SessionDB) InsertSessionRowInTransaction(ctx context.Context, conn SQLiteConn, n NewSession) error {
	return d.insertSessionRowInTransaction(ctx, conn, n)
}
