package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Transaction scoped rewind to the target row for a retry rewrite committed in the re-send admission
func (d *SessionDB) RewindInTransaction(ctx context.Context, conn SQLiteConn, sessionID string, targetMessageID int64, o RewindOptions) (*pyjson.Object, []int64, any, *int64, error) {
	return d.rewindInTransaction(ctx, conn, sessionID, targetMessageID, o)
}
