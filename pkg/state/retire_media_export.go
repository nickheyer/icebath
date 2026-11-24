package state

import (
	"context"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Records the media a payload references as durable deletion candidates inside the caller's write transaction
func RetireMedia(ctx context.Context, conn SQLiteConn, payload *jsonutil.Object, requestID string) error {
	return retireMedia(ctx, conn, payload, requestID)
}
