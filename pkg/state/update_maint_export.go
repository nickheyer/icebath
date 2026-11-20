package state

import "context"

// Whether the store still carries the inline FTS layout the storage optimization replaces
func DBHasLegacyInlineFTS(ctx context.Context, conn SQLiteConn) (bool, error) {
	return dbHasLegacyInlineFTS(ctx, conn)
}
