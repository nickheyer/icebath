package kanban

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// RecordFailureOptions holds the keyword arguments of one non success outcome record
type RecordFailureOptions struct {
	Outcome           string
	FailureLimit      *int64
	ForceTrip         bool
	ReleaseClaim      bool
	EndRun            bool
	EventPayloadExtra *pyjson.Object
	Infrastructure    bool
}

// Records a non success outcome and trips the circuit breaker at the limit, true when the task auto blocked
func recordTaskFailure(ctx context.Context, conn state.SQLiteConn, taskID, errText string, f RecordFailureOptions) (bool, error) {
	limit := int64(DefaultFailureLimit)
	if f.FailureLimit != nil {
		limit = *f.FailureLimit
	}
	if r := []rune(errText); len(r) > 500 {
		errText = string(r[:500])
	}
	blocked := false
	err := WriteTxn(ctx, conn, false, func() error {
		row, err := QueryOne(ctx, conn, "SELECT consecutive_failures, status, max_retries, current_run_id FROM tasks WHERE id = ?", taskID)
		if err != nil || row == nil {
			return err
		}
		var retryStatus string
		if f.ReleaseClaim {
			var runID *int64
			if v := row.Get("current_run_id"); v != nil {
				id := cellInt(v)
				runID = &id
			}
			if retryStatus, err = retryStatusForRun(ctx, conn, taskID, runID); err != nil {
				return err
			}
		} else if row.Get("status") == "review" {
			retryStatus = "review"
		} else {
			retryStatus = "ready"
		}
		failures, err := pyIntOf(row.Get("consecutive_failures"))
		if err != nil {
			return err
		}
		if !f.Infrastructure {
			failures++
		}
		effectiveLimit, limitSource := limit, "dispatcher"
		if override := rowGet(row, "max_retries", nil); override != nil {
			if effectiveLimit, err = pyIntOf(override); err != nil {
				return err
			}
			limitSource = "task"
		}
		if f.Infrastructure || !(f.ForceTrip || failures >= effectiveLimit) {
			if f.ReleaseClaim {
				_, err = conn.ExecContext(ctx, "UPDATE tasks SET status = ?, claim_lock = NULL, claim_expires = NULL, worker_pid = NULL, worker_started_at = NULL, consecutive_failures = ?, last_failure_error = ? WHERE id = ? AND status = 'running'", retryStatus, failures, errText, taskID)
			} else {
				_, err = conn.ExecContext(ctx, "UPDATE tasks SET consecutive_failures = ?, last_failure_error = ? WHERE id = ?", failures, errText, taskID)
			}
			if err != nil || !f.EndRun {
				return err
			}
			detail := payload("failures", failures, "retry_status", retryStatus)
			if f.Infrastructure {
				detail.Set("infrastructure", true)
			}
			runID, err := endRun(ctx, conn, taskID, f.Outcome, nil, &errText, detail, f.Outcome)
			if err != nil {
				return err
			}
			body := payload("error", errText)
			body.Update(detail)
			return appendEvent(ctx, conn, taskID, f.Outcome, body, runID)
		}
		claimReset := ""
		if f.ReleaseClaim {
			claimReset = "claim_lock = NULL, claim_expires = NULL, worker_pid = NULL, worker_started_at = NULL, "
		}
		if _, err = conn.ExecContext(ctx, "UPDATE tasks SET status = 'blocked', "+claimReset+"consecutive_failures = ?, last_failure_error = ? WHERE id = ? AND status IN ('running', 'ready', 'review')", failures, errText, taskID); err != nil {
			return err
		}
		body := payload("failures", failures, "effective_limit", effectiveLimit, "limit_source", limitSource, "error", errText, "trigger_outcome", f.Outcome, "retry_status", retryStatus)
		var runID *int64
		if f.EndRun {
			meta := payload("failures", failures, "trigger_outcome", f.Outcome, "effective_limit", effectiveLimit, "limit_source", limitSource, "retry_status", retryStatus)
			if runID, err = endRun(ctx, conn, taskID, "gave_up", nil, &errText, meta, "gave_up"); err != nil {
				return err
			}
		}
		if f.ForceTrip {
			body.Set("sticky", true)
		}
		if f.EventPayloadExtra != nil && f.EventPayloadExtra.Len() > 0 {
			body.Update(f.EventPayloadExtra)
		}
		blocked = true
		return appendEvent(ctx, conn, taskID, "gave_up", body, runID)
	})
	if err != nil {
		return false, err
	}
	return blocked, nil
}

// RecordTaskFailure is the dispatcher failure funnel other packages call
func RecordTaskFailure(ctx context.Context, conn state.SQLiteConn, taskID, errText string, f RecordFailureOptions) (bool, error) {
	return recordTaskFailure(ctx, conn, taskID, errText, f)
}

// Records the timed out outcome of a kanban worker whose iteration budget ran out
func RecordBudgetExhausted(ctx context.Context, kanbanTask string, apiCallCount, maxIterations int) error {
	conn, err := Connect(ctx, "", nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	extra := payload("budget_used", apiCallCount, "budget_max", maxIterations)
	_, err = recordTaskFailure(ctx, conn, kanbanTask,
		fmt.Sprintf("Iteration budget exhausted (%d/%d) — task could not complete within the allowed iterations", apiCallCount, maxIterations),
		RecordFailureOptions{Outcome: "timed_out", ReleaseClaim: true, EndRun: true, EventPayloadExtra: extra})
	return err
}
