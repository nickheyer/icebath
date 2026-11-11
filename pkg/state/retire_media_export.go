package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Records the media a payload references as durable deletion candidates inside the caller's write transaction
func RetireMedia(ctx context.Context, conn SQLiteConn, payload *pyjson.Object, requestID string) error {
	return retireMedia(ctx, conn, payload, requestID)
}
