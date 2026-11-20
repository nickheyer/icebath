package state

import "context"

// Session ids of one local conversation read on an open connection, its segments and compression lineage
func LocalConversationIDs(ctx context.Context, conn SQLiteConn, sessionID string) ([]string, error) {
	return localConversationIDs(ctx, conn, sessionID)
}
