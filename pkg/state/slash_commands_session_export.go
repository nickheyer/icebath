package state

import (
	"context"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Transaction scoped rewind to the target row for a retry rewrite committed in the re-send admission
func (d *SessionDB) RewindInTransaction(ctx context.Context, conn SQLiteConn, sessionID string, targetMessageID int64, o RewindOptions) (*jsonutil.Object, []int64, any, *int64, error) {
	return d.rewindInTransaction(ctx, conn, sessionID, targetMessageID, o)
}
