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

// Run id standing for a Python int SQLite cannot hold, unequal to every real run and refused when bound
var runIDBeyondSQLite = func() *int64 { v := int64(-1); return &v }()

// Failure sqlite3 raises binding a run id past its integer range, nil for every other id
func runIDBindable(id *int64) error {
	if id == runIDBeyondSQLite {
		return &RuntimeError{"Python int too large to convert to SQLite INTEGER"}
	}
	return nil
}

// Run id of this worker for the task as the board calls take it, nil when unset or not an integer
func workerRunIDArg(taskID string) *int64 {
	raw, _ := ownTaskEnv(taskID, "ICEBATH_KANBAN_RUN_ID")
	if raw == "" {
		return nil
	}
	parsed, ok := pyint.Parse(raw)
	if !ok {
		return nil
	}
	if !parsed.IsInt64() {
		return runIDBeyondSQLite
	}
	id := parsed.Int64()
	return &id
}

// Records this worker's pid on its run, false only when the board says the run was already reclaimed
func RegisterCurrentWorkerFromEnv(ctx context.Context) (registered bool) {
	taskID := os.Getenv("ICEBATH_KANBAN_TASK")
	if taskID == "" {
		return true
	}
	run := workerRunIDArg(taskID)
	if run == nil || isDelegatedChildContext(ctx, tools.ToolEnvFrom(ctx)) {
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
	if err := runIDBindable(run); err != nil {
		_ = conn.Close()
		return fail(err)
	}
	live, err := adoptOnBoard(ctx, conn, taskID, *run)
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
