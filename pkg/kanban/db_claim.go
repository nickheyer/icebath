package kanban

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Decoded payload of an event row, empty for a missing row
func eventPayload(row *Row) *pyjson.Object {
	if row == nil {
		return pyjson.NewObject()
	}
	return jsonDict(rowGet(row, "payload", nil))
}

// Reports whether the newest block event must wait for an operator, an explicit block or a sticky breaker trip
func hasStickyBlock(ctx context.Context, conn state.SQLiteConn, taskID string) (bool, error) {
	row, err := QueryOne(ctx, conn, "SELECT kind FROM task_events WHERE task_id = ? AND kind IN ('blocked', 'unblocked') ORDER BY id DESC LIMIT 1", taskID)
	if err != nil {
		return false, err
	}
	if row != nil && row.Get("kind") == "blocked" {
		return true, nil
	}
	trip, err := QueryOne(ctx, conn, "SELECT payload FROM task_events WHERE task_id = ? AND kind = 'gave_up' AND id > COALESCE("+
		"  (SELECT MAX(id) FROM task_events WHERE task_id = ? AND kind = 'unblocked'), 0) ORDER BY id DESC LIMIT 1", taskID, taskID)
	if err != nil || trip == nil {
		return false, err
	}
	return truthy(objGet(eventPayload(trip), "sticky")), nil
}

// Newest event row of a kind, optionally scoped to one run
func latestEvent(ctx context.Context, conn state.SQLiteConn, taskID, kind string, runID *int64) (*Row, error) {
	query := "SELECT payload FROM task_events WHERE task_id = ? AND kind = ?"
	args := []any{taskID, kind}
	if runID != nil {
		query += " AND run_id = ?"
		args = append(args, *runID)
	}
	return QueryOne(ctx, conn, query+" ORDER BY id DESC LIMIT 1", args...)
}

// Review when the newest lifecycle event carries a review resume, retry or source status, else ready
func resumeStatusFromEvents(ctx context.Context, conn state.SQLiteConn, taskID string) (string, error) {
	row, err := QueryOne(ctx, conn, "SELECT payload FROM task_events WHERE task_id = ? AND kind IN ("+
		"'blocked', 'block_loop_detected', 'dependency_wait', 'gave_up', "+
		"'unblocked', 'changes_requested', 'review_reopened', 'status', 'reclaimed', "+
		"'stale', 'timed_out', 'crashed', 'spawn_failed', 'rate_limited'"+
		") ORDER BY id DESC LIMIT 1", taskID)
	if err != nil {
		return "", err
	}
	p := eventPayload(row)
	for _, key := range []string{"resume_status", "retry_status", "source_status"} {
		if objGet(p, key) == "review" {
			return "review", nil
		}
	}
	return "ready", nil
}

// Promotes todo and non sticky blocked tasks whose parents are all done or archived and returns the count
func RecomputeReady(ctx context.Context, conn state.SQLiteConn, failureLimit *int64) (int, error) {
	limit := int64(DefaultFailureLimit)
	if failureLimit != nil {
		limit = *failureLimit
	}
	promoted := 0
	err := WriteTxn(ctx, conn, false, func() error {
		rows, err := QueryRows(ctx, conn, "SELECT id, status, consecutive_failures, max_retries FROM tasks WHERE status IN ('todo', 'blocked')")
		if err != nil {
			return err
		}
		for _, row := range rows {
			taskID, curStatus := cellStr(row.Get("id")), cellStr(row.Get("status"))
			if curStatus == "blocked" {
				sticky, err := hasStickyBlock(ctx, conn, taskID)
				if err != nil {
					return err
				}
				if sticky {
					continue
				}
			}
			ok, err := parentsSatisfied(ctx, conn, taskID)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			resume, err := resumeStatusFromEvents(ctx, conn, taskID)
			if err != nil {
				return err
			}
			if curStatus == "blocked" {
				failures := int64(0)
				if truthy(row.Get("consecutive_failures")) {
					if failures, err = pyIntOf(row.Get("consecutive_failures")); err != nil {
						return err
					}
				}
				effective := limit
				if v := row.Get("max_retries"); v != nil {
					if effective, err = pyIntOf(v); err != nil {
						return err
					}
				}
				if failures >= effective {
					continue
				}
				_, err = conn.ExecContext(ctx, "UPDATE tasks SET status = ? WHERE id = ? AND status = 'blocked'", resume, taskID)
			} else {
				_, err = conn.ExecContext(ctx, "UPDATE tasks SET status = ? WHERE id = ? AND status = 'todo'", resume, taskID)
			}
			if err != nil {
				return err
			}
			var body any
			if resume != "ready" {
				body = payload("status", resume)
			}
			if err := appendEvent(ctx, conn, taskID, "promoted", body, nil); err != nil {
				return err
			}
			promoted++
		}
		return nil
	})
	return promoted, err
}

// Reports whether every direct parent is done or archived
func parentsSatisfied(ctx context.Context, conn state.SQLiteConn, taskID string) (bool, error) {
	row, err := QueryOne(ctx, conn, "SELECT 1 FROM task_links l JOIN tasks p ON p.id = l.parent_id WHERE l.child_id = ? AND p.status NOT IN ('done', 'archived') LIMIT 1", taskID)
	return row == nil, err
}

// Open parent of a task with its status
type ParentStatus struct {
	ID     string
	Status string
}

// Every direct parent still gating the task with its status in id order
func UnsatisfiedParents(ctx context.Context, conn state.SQLiteConn, taskID string) ([]ParentStatus, error) {
	rows, err := QueryRows(ctx, conn, "SELECT p.id, p.status FROM task_links l JOIN tasks p ON p.id = l.parent_id WHERE l.child_id = ? AND p.status NOT IN ('done', 'archived') ORDER BY p.id", taskID)
	if err != nil {
		return nil, err
	}
	out := make([]ParentStatus, len(rows))
	for i, r := range rows {
		out[i] = ParentStatus{ID: cellStr(r.Get("id")), Status: cellStr(r.Get("status"))}
	}
	return out, nil
}

// Closes a leaked open run before a status flip so current_run_id stays null exactly when the run is terminal
func reclaimDanglingRun(ctx context.Context, conn state.SQLiteConn, taskID string, statuses []string, now int64, note string) error {
	args := []any{taskID}
	for _, s := range statuses {
		args = append(args, s)
	}
	stale, err := QueryOne(ctx, conn, fmt.Sprintf("SELECT current_run_id FROM tasks WHERE id = ? AND status IN (%s)", strings.Join(strings.Split(sqlMarks(len(statuses)), ","), ", ")), args...)
	if err != nil || stale == nil || !truthy(stale.Get("current_run_id")) {
		return err
	}
	runID, err := pyIntOf(stale.Get("current_run_id"))
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `
            UPDATE task_runs
               SET status = 'reclaimed', outcome = 'reclaimed',
                   summary = COALESCE(summary, ?),
                   ended_at = ?,
                   claim_lock = NULL, claim_expires = NULL, worker_pid = NULL
             WHERE id = ? AND ended_at IS NULL
            `, note, now, runID)
	return err
}

// Moves the source status to running under the lock, opens a run and emits claimed, nil when the swap lost
func claimAndOpenRun(ctx context.Context, conn state.SQLiteConn, taskID, sourceStatus, lock string, expires, now int64, eventExtra *pyjson.Object) (*int64, error) {
	res, err := conn.ExecContext(ctx, fmt.Sprintf(`
        UPDATE tasks
           SET status        = 'running',
               claim_lock    = ?,
               claim_expires = ?,
               started_at    = COALESCE(started_at, ?)
         WHERE id = ?
           AND status = '%s'
           AND claim_lock IS NULL
        `, sourceStatus), lock, expires, now, taskID)
	if err != nil || rowsAffected(res) != 1 {
		return nil, err
	}
	trow, err := QueryOne(ctx, conn, "SELECT assignee, max_runtime_seconds, current_step_key FROM tasks WHERE id = ?", taskID)
	if err != nil {
		return nil, err
	}
	var assignee, maxRuntime, stepKey any
	if trow != nil {
		assignee, maxRuntime, stepKey = trow.Get("assignee"), trow.Get("max_runtime_seconds"), trow.Get("current_step_key")
	}
	runRes, err := conn.ExecContext(ctx, `
        INSERT INTO task_runs (
            task_id, profile, step_key, status,
            claim_lock, claim_expires, max_runtime_seconds,
            started_at
        ) VALUES (?, ?, ?, 'running', ?, ?, ?, ?)
        `, taskID, assignee, stepKey, lock, expires, maxRuntime, now)
	if err != nil {
		return nil, err
	}
	runID := lastRowID(runRes)
	if _, err := conn.ExecContext(ctx, "UPDATE tasks SET current_run_id = ? WHERE id = ?", runID, taskID); err != nil {
		return nil, err
	}
	body := payload("lock", lock, "expires", expires, "run_id", runID)
	if eventExtra != nil {
		for _, k := range eventExtra.Keys {
			body.Set(k, eventExtra.Vals[k])
		}
	}
	if err := appendEvent(ctx, conn, taskID, "claimed", body, &runID); err != nil {
		return nil, err
	}
	return &runID, nil
}

// Lock owner of a claim, the caller supplied claimer or this host and process
func claimLockOf(claimer *string) string {
	if claimer != nil && *claimer != "" {
		return *claimer
	}
	return claimerID()
}

// Atomically moves a ready task to running, demoting it to todo when a parent reopened, nil when the claim lost
func ClaimTask(ctx context.Context, conn state.SQLiteConn, taskID string, ttlSeconds *int64, claimer *string) (*Task, error) {
	now := time.Now().Unix()
	lock := claimLockOf(claimer)
	expires := now + resolveClaimTTLSeconds(ttlSeconds)
	var claimed *Task
	var runID *int64
	err := WriteTxn(ctx, conn, false, func() error {
		ok, err := parentsSatisfied(ctx, conn, taskID)
		if err != nil {
			return err
		}
		if !ok {
			if _, err := conn.ExecContext(ctx, "UPDATE tasks SET status = 'todo' WHERE id = ? AND status = 'ready'", taskID); err != nil {
				return err
			}
			return appendEvent(ctx, conn, taskID, "claim_rejected", payload("reason", "parents_not_done"), nil)
		}
		if err := reclaimDanglingRun(ctx, conn, taskID, []string{"ready"}, now, "invariant recovery on re-claim"); err != nil {
			return err
		}
		if runID, err = claimAndOpenRun(ctx, conn, taskID, "ready", lock, expires, now, nil); err != nil || runID == nil {
			return err
		}
		claimed, err = getTask(ctx, conn, taskID)
		return err
	})
	if err != nil || runID == nil {
		return nil, err
	}
	fireTaskHook(ctx, "kanban_task_claimed", claimed, taskID, runID, nil)
	return claimed, nil
}

// Atomically moves a review task to running under a new reviewer run, demoting it when a parent reopened
func ClaimReviewTask(ctx context.Context, conn state.SQLiteConn, taskID string, ttlSeconds *int64, claimer *string) (*Task, error) {
	now := time.Now().Unix()
	lock := claimLockOf(claimer)
	expires := now + resolveClaimTTLSeconds(ttlSeconds)
	var claimed *Task
	err := WriteTxn(ctx, conn, false, func() error {
		ok, err := parentsSatisfied(ctx, conn, taskID)
		if err != nil {
			return err
		}
		if !ok {
			res, err := conn.ExecContext(ctx, "UPDATE tasks SET status = 'todo' WHERE id = ? AND status = 'review' AND claim_lock IS NULL", taskID)
			if err != nil {
				return err
			}
			if rowsAffected(res) == 1 {
				return appendEvent(ctx, conn, taskID, "dependency_wait", payload("reason", "parent_reopened", "source_status", "review"), nil)
			}
			return nil
		}
		runID, err := claimAndOpenRun(ctx, conn, taskID, "review", lock, expires, now, payload("source_status", "review"))
		if err != nil || runID == nil {
			return err
		}
		claimed, err = getTask(ctx, conn, taskID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// Review when the run was claimed from review, else ready, so retries never turn a reviewer run into an implementation run
func retryStatusForRun(ctx context.Context, conn state.SQLiteConn, taskID string, runID *int64) (string, error) {
	if runID == nil {
		var err error
		if runID, err = currentRunID(ctx, conn, taskID); err != nil {
			return "", err
		}
	}
	if runID == nil {
		return "ready", nil
	}
	event, err := latestEvent(ctx, conn, taskID, "claimed", runID)
	if err != nil {
		return "", err
	}
	if objGet(eventPayload(event), "source_status") == "review" {
		return "review", nil
	}
	return "ready", nil
}

// Task status a run outcome hands off to
var runOutcomeTerminalStatus = map[string]string{
	"completed":         "done",
	"review_requested":  "review",
	"changes_requested": "changes_requested",
	"blocked":           "blocked",
	"dependency_wait":   "blocked",
}

// Lifecycle status as one run sees it, superseded once another run owns the task
func GoalRunStatus(ctx context.Context, conn state.SQLiteConn, taskID string, expectedRunID *int64) (*string, error) {
	task, err := getTask(ctx, conn, taskID)
	if err != nil || task == nil {
		return nil, err
	}
	str := func(s string) (*string, error) { return &s, nil }
	if expectedRunID != nil {
		row, err := QueryOne(ctx, conn, "SELECT outcome FROM task_runs WHERE id = ? AND task_id = ?", *expectedRunID, taskID)
		if err != nil {
			return nil, err
		}
		var outcome *string
		if row != nil && row.Get("outcome") != nil {
			o := fmt.Sprint(LossyText(row.Get("outcome")))
			outcome = &o
		}
		if outcome != nil {
			if terminal, ok := runOutcomeTerminalStatus[*outcome]; ok {
				return str(terminal)
			}
		}
		if outcome != nil || task.CurrentRunID == nil || *task.CurrentRunID != *expectedRunID {
			return str("superseded")
		}
	}
	if task.Status == "ready" || task.Status == "todo" {
		event, err := QueryOne(ctx, conn, "SELECT kind FROM task_events WHERE task_id = ? ORDER BY id DESC LIMIT 1", taskID)
		if err != nil {
			return nil, err
		}
		if event != nil && event.Get("kind") == "changes_requested" {
			return str("changes_requested")
		}
	}
	return str(task.Status)
}

// Extends a running claim the caller still owns, mirroring the expiry onto its run
func HeartbeatClaim(ctx context.Context, conn state.SQLiteConn, taskID string, ttlSeconds *int64, claimer *string) (bool, error) {
	expires := time.Now().Unix() + resolveClaimTTLSeconds(ttlSeconds)
	lock := claimLockOf(claimer)
	owned := false
	err := WriteTxn(ctx, conn, false, func() error {
		res, err := conn.ExecContext(ctx, "UPDATE tasks SET claim_expires = ? WHERE id = ? AND status = 'running' AND claim_lock = ?", expires, taskID, lock)
		if err != nil || rowsAffected(res) != 1 {
			return err
		}
		owned = true
		_, err = extendRunClaim(ctx, conn, taskID, expires)
		return err
	})
	return owned && err == nil, err
}

// Mirrors a task claim extension onto its active run and returns that run id
func extendRunClaim(ctx context.Context, conn state.SQLiteConn, taskID string, expires int64) (*int64, error) {
	runID, err := currentRunID(ctx, conn, taskID)
	if err != nil || runID == nil {
		return nil, err
	}
	_, err = conn.ExecContext(ctx, "UPDATE task_runs SET claim_expires = ? WHERE id = ?", expires, *runID)
	return runID, err
}

// Reclaims running tasks whose claim expired, extending live host local workers and booking every release as a failure
func ReleaseStaleClaims(ctx context.Context, conn state.SQLiteConn, signalFn SignalFunc, failureLimit *int64) (int, error) {
	now := time.Now().Unix()
	reclaimed := 0
	prefix := hostPrefix()
	stale, err := QueryRows(ctx, conn, "SELECT id, claim_lock, worker_pid, worker_started_at, claim_expires, last_heartbeat_at, "+
		"       assignee FROM tasks WHERE status = 'running' AND claim_expires IS NOT NULL   AND claim_expires < ?", now)
	if err != nil {
		return 0, err
	}
	for _, row := range stale {
		taskID := cellStr(row.Get("id"))
		blocked, err := ownerReclaimBlocked(ctx, conn, taskID)
		if err != nil {
			return reclaimed, err
		}
		if blocked {
			continue
		}
		claimLock := cellOptStr(row.Get("claim_lock"))
		hostLocal := claimLock != nil && strings.HasPrefix(*claimLock, prefix)
		hb := toEpoch(row.Get("last_heartbeat_at"))
		heartbeatStale := hb != nil && now-*hb > DefaultClaimHeartbeatMaxStaleSeconds
		startedAt := rowGet(row, "worker_started_at", nil)
		pid := row.Get("worker_pid")
		if hostLocal && truthy(pid) && workerAlive(pid, startedAt) && !heartbeatStale {
			if err := extendLiveStaleClaim(ctx, conn, row, now); err != nil {
				return reclaimed, err
			}
			continue
		}
		termination := terminateReclaimedWorker(ctx, pid, claimLock, signalFn, startedAt)
		if workerSurvivedTermination(termination) {
			if err := deferReclaimForLiveWorker(ctx, conn, taskID, claimLock, now, termination, "ttl_expired_worker_alive"); err != nil {
				return reclaimed, err
			}
			continue
		}
		pidInt, err := optInt(pid)
		if err != nil {
			return reclaimed, err
		}
		var retryStatus, errText string
		var runID *int64
		released := false
		err = WriteTxn(ctx, conn, false, func() error {
			attempted, _ := termination.Get("termination_attempted")
			skip, err := ownerReclaimBlockedAfterKill(ctx, conn, taskID, truthy(attempted))
			if err != nil || skip {
				return err
			}
			if retryStatus, err = retryStatusForRun(ctx, conn, taskID, nil); err != nil {
				return err
			}
			if errText, err = reclaimError(ctx, conn, taskID, claimLock); err != nil {
				return err
			}
			res, err := conn.ExecContext(ctx, "UPDATE tasks SET status = ?, claim_lock = NULL, claim_expires = NULL, worker_pid = NULL, worker_started_at = NULL "+
				"WHERE id = ? AND status = 'running' AND claim_lock IS ? AND claim_expires IS NOT NULL AND claim_expires < ? AND worker_pid IS ?",
				retryStatus, taskID, optStrValue(claimLock), now, pid)
			if err != nil || rowsAffected(res) != 1 {
				return err
			}
			runID, err = recordReclaim(ctx, conn, taskID, termination, errText, payload(
				"stale_lock", claimLock, "worker_pid", pidInt, "claim_expires", toEpoch(row.Get("claim_expires")),
				"last_heartbeat_at", hb, "now", now, "host_local", hostLocal, "heartbeat_stale", heartbeatStale,
				"retry_status", retryStatus))
			released = err == nil
			return err
		})
		if err != nil {
			return reclaimed, err
		}
		if !released {
			continue
		}
		reclaimed++
		limit := failureLimit
		if _, err := recordTaskFailure(ctx, conn, taskID, errText, RecordFailureOptions{Outcome: "reclaimed", FailureLimit: limit,
			EventPayloadExtra: payload("worker_pid", pidInt, "retry_status", retryStatus)}); err != nil {
			return reclaimed, err
		}
		if kanbanObserverConsumed(ctx, "on_kanban_worker_stale_claim") {
			fireKanbanLifecycleHook(ctx, "on_kanban_worker_stale_claim", taskID, map[string]any{
				"board": GetCurrentBoard(ctx), "assignee": row.Get("assignee"), "run_id": optIntValue(runID),
				"worker_pid": optIntValue(pidInt), "heartbeat_stale": heartbeatStale, "retry_status": retryStatus,
			})
		}
	}
	return reclaimed, nil
}

// Closes the active run as reclaimed and emits the reclaimed event merged with the termination report
func recordReclaim(ctx context.Context, conn state.SQLiteConn, taskID string, termination *pyjson.Object, errText string, body *pyjson.Object) (*int64, error) {
	runID, err := endRun(ctx, conn, taskID, "reclaimed", nil, &errText, termination, "reclaimed")
	if err != nil {
		return nil, err
	}
	if termination != nil {
		for _, k := range termination.Keys {
			body.Set(k, termination.Vals[k])
		}
	}
	return runID, appendEvent(ctx, conn, taskID, "reclaimed", body, runID)
}

// Extends an expired claim whose host local worker is alive, swapping on the same expired lock
func extendLiveStaleClaim(ctx context.Context, conn state.SQLiteConn, row *Row, now int64) error {
	newExpires := now + resolveClaimTTLSeconds(nil)
	taskID := cellStr(row.Get("id"))
	return WriteTxn(ctx, conn, false, func() error {
		res, err := conn.ExecContext(ctx, "UPDATE tasks SET claim_expires = ? WHERE id = ? AND status = 'running'   AND claim_lock IS ?   AND claim_expires IS NOT NULL   AND claim_expires < ?",
			newExpires, taskID, row.Get("claim_lock"), now)
		if err != nil || rowsAffected(res) != 1 {
			return err
		}
		runID, err := extendRunClaim(ctx, conn, taskID, newExpires)
		if err != nil {
			return err
		}
		pid, err := pyIntOf(row.Get("worker_pid"))
		if err != nil {
			return err
		}
		return appendEvent(ctx, conn, taskID, "claim_extended", payload(
			"reason", "pid_alive", "worker_pid", pid, "claim_lock", row.Get("claim_lock"),
			"claim_expires_was", toEpoch(row.Get("claim_expires")), "claim_expires_now", newExpires,
			"last_heartbeat_at", toEpoch(row.Get("last_heartbeat_at"))), runID)
	})
}

// Operator reclaim regardless of TTL restoring the source phase and resetting the failure counter, false when not running
func ReclaimTask(ctx context.Context, conn state.SQLiteConn, taskID string, reason *string, signalFn SignalFunc) (bool, error) {
	row, err := QueryOne(ctx, conn, "SELECT status, claim_lock, worker_pid, worker_started_at FROM tasks WHERE id = ?", taskID)
	if err != nil || row == nil {
		return false, err
	}
	if row.Get("status") != "running" && row.Get("claim_lock") == nil {
		return false, nil
	}
	prevLock := cellOptStr(row.Get("claim_lock"))
	termination := terminateReclaimedWorker(ctx, row.Get("worker_pid"), prevLock, signalFn, row.Get("worker_started_at"))
	released := false
	err = WriteTxn(ctx, conn, false, func() error {
		retryStatus, err := retryStatusForRun(ctx, conn, taskID, nil)
		if err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, "UPDATE tasks SET status = ?, claim_lock = NULL, claim_expires = NULL, worker_pid = NULL, worker_started_at = NULL "+
			"WHERE id = ? AND status IN ('running', 'ready', 'blocked') AND claim_lock IS ?", retryStatus, taskID, optStrValue(prevLock))
		if err != nil || rowsAffected(res) != 1 {
			return err
		}
		errText := "manual_reclaim lock=" + pyStrOpt(prevLock)
		if reason != nil && *reason != "" {
			errText = "manual_reclaim: " + *reason
		}
		_, err = recordReclaim(ctx, conn, taskID, termination, errText, payload("manual", true, "reason", reason, "prev_lock", prevLock, "retry_status", retryStatus))
		released = err == nil
		return err
	})
	if err != nil || !released {
		return false, err
	}
	if err := clearFailureCounter(ctx, conn, taskID); err != nil {
		return false, err
	}
	return true, nil
}

// Python str of an optional string, None for nil
func pyStrOpt(s *string) string {
	if s == nil {
		return "None"
	}
	return *s
}

// Reassigns a task, releasing a running claim first when asked, false when the task stays claimed
func ReassignTask(ctx context.Context, conn state.SQLiteConn, taskID string, profile *string, reclaimFirst bool, reason *string) (bool, error) {
	if reclaimFirst {
		r := "reassign"
		if reason != nil && *reason != "" {
			r = *reason
		}
		if _, err := ReclaimTask(ctx, conn, taskID, &r, nil); err != nil {
			return false, err
		}
	}
	ok, err := AssignTask(ctx, conn, taskID, profile)
	if _, refused := err.(*RuntimeError); refused {
		return false, nil
	}
	return ok, err
}

// Splits claimed card ids into those this worker created or linked as children and the phantom rest
func verifyCreatedCards(ctx context.Context, conn state.SQLiteConn, completingTaskID string, claimedIDs []string) ([]string, []string, error) {
	var ordered []any
	seen := map[string]bool{}
	for _, x := range claimedIDs {
		if s := pystr.Strip(x); s != "" && !seen[s] {
			seen[s] = true
			ordered = append(ordered, s)
		}
	}
	if len(ordered) == 0 {
		return []string{}, []string{}, nil
	}
	row, err := QueryOne(ctx, conn, "SELECT assignee FROM tasks WHERE id = ?", completingTaskID)
	if err != nil {
		return nil, nil, err
	}
	if row == nil {
		out := make([]string, len(ordered))
		for i, v := range ordered {
			out[i] = v.(string)
		}
		return []string{}, out, nil
	}
	assignee := cellOptStr(row.Get("assignee"))
	rows, err := QueryRows(ctx, conn, "SELECT id, created_by FROM tasks WHERE id IN ("+sqlMarks(len(ordered))+")", ordered...)
	if err != nil {
		return nil, nil, err
	}
	found := map[string]*string{}
	for _, r := range rows {
		found[cellStr(r.Get("id"))] = cellOptStr(r.Get("created_by"))
	}
	children, err := ChildIDs(ctx, conn, completingTaskID)
	if err != nil {
		return nil, nil, err
	}
	linked := map[string]bool{}
	for _, c := range children {
		linked[c] = true
	}
	verified, phantom := []string{}, []string{}
	for _, v := range ordered {
		cid := v.(string)
		createdBy := found[cid]
		trusted := createdBy != nil && ((assignee != nil && *assignee != "" && *createdBy == *assignee) || *createdBy == completingTaskID || linked[cid])
		if trusted {
			verified = append(verified, cid)
		} else {
			phantom = append(phantom, cid)
		}
	}
	return verified, phantom, nil
}

// Task id references in prose, matched on Unicode word boundaries like the re module
var taskIDProseRE = regexp2.MustCompile(`\bt_[a-f0-9]{8,}\b`, regexp2.None)

// Task id references in the text that resolve to no task, deduplicated in first seen order
func scanProseForPhantomIDs(ctx context.Context, conn state.SQLiteConn, text string) ([]string, error) {
	if text == "" {
		return []string{}, nil
	}
	var ids []string
	seen := map[string]bool{}
	m, err := taskIDProseRE.FindStringMatch(text)
	for err == nil && m != nil {
		if s := m.String(); !seen[s] {
			seen[s] = true
			ids = append(ids, s)
		}
		m, err = taskIDProseRE.FindNextMatch(m)
	}
	if err != nil {
		return nil, err
	}
	return missingTaskIDs(ctx, conn, ids)
}

// Completion refused for created cards that do not exist or this worker did not create, unwrapping to ValueError
type HallucinatedCardsError struct {
	Phantom          []string
	CompletingTaskID string
}

func (e *HallucinatedCardsError) Error() string {
	return "completion blocked: claimed created_cards that do not exist or were not created by this worker: " + strings.Join(e.Phantom, ", ")
}
func (e *HallucinatedCardsError) Unwrap() error { return &ValueError{e.Error()} }

// Completion refused for a task with no result or summary evidence, unwrapping to ValueError
type EmptyCompletionError struct{ TaskID string }

func (e *EmptyCompletionError) Error() string {
	return fmt.Sprintf("completion blocked: %s has no result or summary evidence", e.TaskID)
}
func (e *EmptyCompletionError) Unwrap() error { return &ValueError{e.Error()} }

// Declared scratch deliverable that could not be preserved, unwrapping to RuntimeError
type ArtifactPreservationError struct{ Msg string }

func (e *ArtifactPreservationError) Error() string { return e.Msg }
func (e *ArtifactPreservationError) Unwrap() error { return &RuntimeError{e.Msg} }

// Completion refused while a live worker claim protects the run, unwrapping to ValueError
type LiveClaimError struct{ TaskID string }

func (e *LiveClaimError) Error() string {
	return e.TaskID + " is running under a live worker claim; pass expected_run_id (worker ownership) or force=True (explicit operator override) instead of closing the live run"
}
func (e *LiveClaimError) Unwrap() error { return &ValueError{e.Error()} }

// Reports whether a running task claim still protects a worker process that exists
func claimIsLive(trow *Row) bool {
	return trow.Get("status") == "running" && trow.Get("claim_lock") != nil && truthy(trow.Get("worker_pid")) &&
		workerAlive(trow.Get("worker_pid"), trow.Get("worker_started_at"))
}
