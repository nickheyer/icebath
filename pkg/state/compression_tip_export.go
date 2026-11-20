package state

import "context"

// Compression tip of a session walked on an already open connection
func CompressionTipOn(ctx context.Context, conn SQLiteConn, sessionID string) (string, error) {
	return compressionTipOn(ctx, conn, sessionID)
}
