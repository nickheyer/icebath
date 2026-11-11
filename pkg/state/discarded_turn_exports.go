package state

import "context"

// Physical transcript a local session's turns write to, resolved on the caller's connection
func LocalPhysicalTarget(ctx context.Context, conn SQLiteConn, sessionID string) (string, error) {
	return localPhysicalTarget(ctx, conn, sessionID)
}

// Compression lineage of a session from its root to its tip, read on the caller's connection
func CanonicalChain(ctx context.Context, conn SQLiteConn, sessionID string) ([]string, error) {
	return canonicalChain(ctx, conn, sessionID)
}

// Stored column value of a message's display metadata
func EncodeDisplayMetadata(displayMetadata any) (any, error) {
	return encodeDisplayMetadata(displayMetadata)
}

// Appends messages to a session inside the caller's write transaction, returning how many rows it wrote
func (d *SessionDB) AppendMessagesInTransaction(ctx context.Context, conn SQLiteConn, sessionID string, messages []map[string]any, guards AppendGuards) (int, error) {
	return d.appendMessagesInTransaction(ctx, conn, sessionID, messages, guards)
}
