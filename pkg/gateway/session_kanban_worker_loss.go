//go:build !icebath_core && !icebath_noextras

package gateway

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Appends a run scoped worker_lost event for a card attempt, dropped once the card moved past the run, never failing
func recordWorkerLoss(ctx context.Context, policy *LocalSessionPolicy, reason string, pid any, bound bool) {
	if err := writeWorkerLoss(ctx, policy, reason, pid, bound); err != nil {
		pkgLog().Error(fmt.Sprintf("kanban worker_lost record failed: %s", err))
	}
}

// Board write of a lost attempt, the error of any failing step
func writeWorkerLoss(ctx context.Context, policy *LocalSessionPolicy, reason string, pid any, bound bool) error {
	text := "null"
	if policy.KanbanJSON != nil && *policy.KanbanJSON != "" {
		text = *policy.KanbanJSON
	}
	decoded, err := jsonutil.Decode(text)
	if err != nil {
		return err
	}
	if !anyutil.IsTrue(decoded) {
		return nil
	}
	context_, isDict := decoded.(*jsonutil.Object)
	if !isDict {
		return &PyError{Kind: "TypeError", Msg: "the kanban worker record is " + errchain.Kind(decoded) + ", expected a mapping"}
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
	runID, err := anyutil.ToInt(rawRun)
	if err != nil {
		return err
	}
	db, err := field("db")
	if err != nil {
		return err
	}
	return kanban.ConnectClosing(ctx, anyutil.String(db), nil, func(conn state.SQLiteConn) error {
		return kanban.WriteTxn(ctx, conn, false, func() error {
			taskID, err := field("task_id")
			if err != nil {
				return err
			}
			row, err := state.ConnRow(ctx, conn, "SELECT status, current_run_id FROM tasks WHERE id=?", taskID)
			if err != nil {
				return err
			}
			if row == nil || row["status"] != "running" || !anyutil.Equal(row["current_run_id"], runID) {
				return nil
			}
			claim, err := field("claim_lock")
			if err != nil {
				return err
			}
			return kanban.AppendTaskEvent(ctx, conn, anyutil.String(taskID), "worker_lost",
				jsonutil.ObjectOf("reason", textutil.First(reason, 200), "claim_lock", claim, "run_id", runID, "pid", pid, "bound", bound), &runID)
		})
	})
}
