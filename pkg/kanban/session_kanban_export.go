package kanban

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
)

// Normalized board slug, a ValueError for an empty or invalid one
func RequireSlug(slug string) (string, error) { return requireSlug(slug) }

// Retry status the dispatcher reads for a run, review for a reviewer attempt
func RetryStatusForRun(ctx context.Context, conn state.SQLiteConn, taskID string, runID *int64) (string, error) {
	return retryStatusForRun(ctx, conn, taskID, runID)
}

// Appends one task event with its JSON payload, bound to the run when given
func AppendEvent(ctx context.Context, conn state.SQLiteConn, taskID, kind string, body any, runID *int64) error {
	return appendEvent(ctx, conn, taskID, kind, body, runID)
}
