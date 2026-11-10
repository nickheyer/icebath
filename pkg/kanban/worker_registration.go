package kanban

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var kanbanToolsLog = slog.Default().With("logger", "tools.kanban_tools")

// Environment value only when this worker is scoped to the task, not ok otherwise
func ownTaskEnv(taskID, name string) (string, bool) {
	if os.Getenv("ICEBATH_KANBAN_TASK") != taskID {
		return "", false
	}
	return os.LookupEnv(name)
}

// Dispatcher run id of this worker when it is scoped to the task, not ok when unset or not an integer
func workerRunID(taskID string) (int64, bool) {
	raw, _ := ownTaskEnv(taskID, "ICEBATH_KANBAN_RUN_ID")
	if raw == "" {
		return 0, false
	}
	parsed, ok := pyint.Parse(raw)
	if !ok || !parsed.IsInt64() {
		return 0, false
	}
	return parsed.Int64(), true
}

// Records this worker's pid on its run, false only when the board says the run was already reclaimed
func RegisterCurrentWorkerFromEnv(ctx context.Context) (registered bool) {
	taskID := os.Getenv("ICEBATH_KANBAN_TASK")
	if taskID == "" {
		return true
	}
	runID, ok := workerRunID(taskID)
	if !ok || tools.IsDelegatedChildContext(ctx) {
		return true
	}
	fail := func(cause any) bool {
		kanbanToolsLog.Debug(fmt.Sprintf("kanban worker registration for %s failed", taskID), "exc_info", fmt.Sprint(cause))
		return true
	}
	defer func() {
		if r := recover(); r != nil {
			registered = fail(r)
		}
	}()
	conn, err := Connect(ctx, "", nil)
	if err != nil {
		return fail(err)
	}
	live, err := adoptOnBoard(ctx, conn, taskID, runID)
	if err != nil {
		return fail(err)
	}
	return live
}

// Adopts the worker pid on an open board and closes it, a close failure ignored
func adoptOnBoard(ctx context.Context, conn state.SQLiteConn, taskID string, runID int64) (bool, error) {
	defer func() { _ = conn.Close() }()
	return AdoptWorkerPid(ctx, conn, taskID, runID, os.Getpid())
}
