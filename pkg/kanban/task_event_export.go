package kanban

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
)

// Inserts an event row inside the caller transaction, a nil run id marking it task scoped
func AppendTaskEvent(ctx context.Context, conn state.SQLiteConn, taskID, kind string, body any, runID *int64) error {
	return appendEvent(ctx, conn, taskID, kind, body, runID)
}
