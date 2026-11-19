package kanban

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
)

// One run by id, nil when absent
func GetRun(ctx context.Context, conn state.SQLiteConn, runID int64) (*Run, error) {
	row, err := QueryOne(ctx, conn, "SELECT * FROM task_runs WHERE id = ?", runID)
	if err != nil || row == nil {
		return nil, err
	}
	return RunFromRow(row)
}
