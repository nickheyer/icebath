package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
)

// Appends a run scoped worker_lost event for a card attempt, dropped once the card moved past the run, never failing
func recordWorkerLoss(ctx context.Context, policy *LocalSessionPolicy, reason string, pid any, bound bool) {
	if err := writeWorkerLoss(ctx, policy, reason, pid, bound); err != nil {
		slog.Error(fmt.Sprintf("kanban worker_lost record failed: %s", err))
	}
}

// Board write of a lost attempt, the error of any failing step
func writeWorkerLoss(ctx context.Context, policy *LocalSessionPolicy, reason string, pid any, bound bool) error {
	text := "null"
	if policy.KanbanJSON != nil && *policy.KanbanJSON != "" {
		text = *policy.KanbanJSON
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return err
	}
	if !pyrepr.Truthy(decoded) {
		return nil
	}
	context_, isDict := decoded.(*pyjson.Object)
	if !isDict {
		return &PyError{Kind: "TypeError", Msg: "'" + pyTypeName(decoded) + "' object is not subscriptable"}
	}
	field := func(key string) (any, error) {
		value, ok := context_.Get(key)
		if !ok {
			return nil, &pyKeyError{key}
		}
		return value, nil
	}
	rawRun, err := field("run_id")
	if err != nil {
		return err
	}
	runID, err := pyint.Int64(rawRun)
	if err != nil {
		return err
	}
	db, err := field("db")
	if err != nil {
		return err
	}
	return kanban.ConnectClosing(ctx, pyrepr.Str(db), nil, func(conn state.SQLiteConn) error {
		return kanban.WriteTxn(ctx, conn, false, func() error {
			taskID, err := field("task_id")
			if err != nil {
				return err
			}
			row, err := state.ConnRow(ctx, conn, "SELECT status, current_run_id FROM tasks WHERE id=?", taskID)
			if err != nil {
				return err
			}
			if row == nil || row["status"] != "running" || !pyrepr.Equal(row["current_run_id"], runID) {
				return nil
			}
			claim, err := field("claim_lock")
			if err != nil {
				return err
			}
			return kanban.AppendTaskEvent(ctx, conn, pyrepr.Str(taskID), "worker_lost",
				pyjson.ObjectOf("reason", pystr.Slice(reason, 0, 200), "claim_lock", claim, "run_id", runID, "pid", pid, "bound", bound), &runID)
		})
	})
}
