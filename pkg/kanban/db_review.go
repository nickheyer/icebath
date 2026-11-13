package kanban

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seams over run bookkeeping and worker termination that tests replace like the hermes monkeypatches
var (
	endOrSynthesizeRunFn       = endOrSynthesizeRun
	terminateReclaimedWorkerFn = terminateReclaimedWorker
)

// EditTaskOptions holds the keyword arguments of a task edit
type EditTaskOptions struct {
	Title    *string
	Body     *string
	Priority *int64
	Result   *string
	Summary  *string
	Metadata *pyjson.Object
	Board    *string
}

// Edits task fields, a result backfill allowed only on a done task, and notifies the changed fields
func EditTask(ctx context.Context, conn state.SQLiteConn, taskID string, o EditTaskOptions) (bool, error) {
	var changedFields []string
	if o.Title != nil {
		changedFields = append(changedFields, "title")
	}
	if o.Body != nil {
		changedFields = append(changedFields, "body")
	}
	if o.Priority != nil {
		changedFields = append(changedFields, "priority")
	}
	edited := false
	err := WriteTxn(ctx, conn, false, func() error {
		status, err := taskStatusOf(ctx, conn, taskID)
		if err != nil {
			return err
		}
		if status == nil || (o.Result != nil && *status != "done") {
			return nil
		}
		var assignments []string
		var params []any
		if o.Title != nil {
			assignments = append(assignments, "title = ?")
			params = append(params, *o.Title)
		}
		if o.Body != nil {
			assignments = append(assignments, "body = ?")
			params = append(params, *o.Body)
		}
		if o.Priority != nil {
			assignments = append(assignments, "priority = ?")
			params = append(params, *o.Priority)
		}
		if o.Result != nil {
			assignments = append(assignments, "result = ?")
			params = append(params, *o.Result)
			changedFields = append(changedFields, "result")
		}
		if len(assignments) == 0 {
			return nil
		}
		params = append(params, taskID)
		if _, err := conn.ExecContext(ctx, "UPDATE tasks SET "+strings.Join(assignments, ", ")+" WHERE id = ?", params...); err != nil {
			return err
		}
		if o.Priority != nil {
			if err := appendEvent(ctx, conn, taskID, "reprioritized", payload("priority", *o.Priority), nil); err != nil {
				return err
			}
		}
		if o.Result == nil {
			var nonPriority []string
			for _, f := range changedFields {
				if f != "priority" {
					nonPriority = append(nonPriority, f)
				}
			}
			if len(nonPriority) > 0 {
				if err := appendEvent(ctx, conn, taskID, "edited", payload("fields", nonPriority), nil); err != nil {
					return err
				}
			}
			edited = true
			return nil
		}
		handoffSummary := o.Result
		if o.Summary != nil {
			handoffSummary = o.Summary
		}
		changedFields = append(changedFields, "summary")
		if o.Metadata != nil {
			changedFields = append(changedFields, "metadata")
		}
		run, err := QueryOne(ctx, conn, `
            SELECT id FROM task_runs
             WHERE task_id = ?
               AND outcome = 'completed'
             ORDER BY COALESCE(ended_at, started_at, 0) DESC, id DESC
             LIMIT 1
            `, taskID)
		if err != nil {
			return err
		}
		var runID int64
		if run == nil {
			if runID, err = synthesizeEndedRun(ctx, conn, taskID, "completed", handoffSummary, nil, metaAny(o.Metadata), runProfile{}); err != nil {
				return err
			}
		} else {
			if runID, err = pyIntOf(run.Get("id")); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "UPDATE task_runs SET summary = ? WHERE id = ?", optStrValue(handoffSummary), runID); err != nil {
				return err
			}
			if o.Metadata != nil {
				text, err := pyjson.Dumps(o.Metadata, pyjson.EnsureASCII(false))
				if err != nil {
					return err
				}
				if _, err := conn.ExecContext(ctx, "UPDATE task_runs SET metadata = ? WHERE id = ?", text, runID); err != nil {
					return err
				}
			}
		}
		fields := []string{"result", "summary"}
		if o.Metadata != nil {
			fields = append(fields, "metadata")
		}
		resultLen := 0
		if truthyStr(o.Result) {
			resultLen = len([]rune(*o.Result))
		}
		edited = true
		return appendEvent(ctx, conn, taskID, "edited",
			payload("fields", fields, "result_len", resultLen, "summary", firstLineOrNil(handoffSummary, 400)), &runID)
	})
	if err != nil || !edited {
		return false, err
	}
	NotifyTaskUpdated(ctx, conn, taskID, changedFields, strOrEmpty(o.Board))
	return true, nil
}

// Blocks a running or ready task with the worker reason under the run it owns
func BlockTask(ctx context.Context, conn state.SQLiteConn, taskID, reason string, expectedRunID *int64) error {
	_, err := BlockTaskWithKind(ctx, conn, taskID, &reason, nil, expectedRunID)
	return err
}

// Moves running or ready to blocked, todo or triage by kind, classifying an untyped breaker block in place
func BlockTaskWithKind(ctx context.Context, conn state.SQLiteConn, taskID string, reason, kind *string, expectedRunID *int64) (bool, error) {
	if kind != nil && !ValidBlockKinds[*kind] {
		return false, &ValueError{fmt.Sprintf("block kind must be one of %s or None", sortedRepr(ValidBlockKinds))}
	}
	blocked, transitioned := false, false
	var blockedTask *Task
	var runID *int64
	err := WriteTxn(ctx, conn, false, func() error {
		curRow, err := QueryOne(ctx, conn, "SELECT status, block_kind, block_recurrences FROM tasks WHERE id = ?", taskID)
		if err != nil || curRow == nil {
			return err
		}
		if curRow.Get("status") == "blocked" {
			if kind == nil || expectedRunID != nil || rowGet(curRow, "block_kind", nil) != nil {
				return nil
			}
			res, err := conn.ExecContext(ctx, "UPDATE tasks SET block_kind = ?, block_recurrences = 1 "+
				"WHERE id = ? AND status = 'blocked' AND block_kind IS NULL "+
				"AND current_run_id IS NULL", *kind, taskID)
			if err != nil {
				return err
			}
			if rowsAffected(res) != 1 {
				return nil
			}
			blocked = true
			return appendEvent(ctx, conn, taskID, "blocked", payload("kind", *kind, "reason", optStrValue(reason), "classified_in_place", true), nil)
		}
		sourceStatus := "ready"
		if curRow.Get("status") == "running" {
			if sourceStatus, err = retryStatusForRun(ctx, conn, taskID, nil); err != nil {
				return err
			}
		}
		requestedKind := kind
		rekindReason := ""
		if kind != nil && *kind == "dependency" {
			satisfied, err := parentsSatisfied(ctx, conn, taskID)
			if err != nil {
				return err
			}
			if satisfied {
				needsInput := "needs_input"
				kind = &needsInput
				rekindReason = "no_open_parent"
			}
		}
		prevRecurrences, err := pyIntOf(orZero(rowGet(curRow, "block_recurrences", nil)))
		if err != nil {
			return err
		}
		route := routeBlock(kind, reason, sourceStatus, cellOptStr(rowGet(curRow, "block_kind", nil)), prevRecurrences)
		if rekindReason != "" {
			route.Payload.Set("requested_kind", optStrValue(requestedKind))
			route.Payload.Set("rekind_reason", rekindReason)
		}
		sql := fmt.Sprintf(`
                UPDATE tasks
                   SET status        = '%s',
                       claim_lock    = NULL,
                       claim_expires = NULL,
                       worker_pid    = NULL,
                       %s
                 WHERE id = ?
                   AND status IN ('running', 'ready')
                `, route.NewStatus, route.SetSQL)
		params := append(append([]any{}, route.Params...), taskID)
		if expectedRunID != nil {
			if err := runIDBindable(expectedRunID); err != nil {
				return err
			}
			sql += " AND current_run_id = ?"
			params = append(params, *expectedRunID)
		}
		res, err := conn.ExecContext(ctx, sql, params...)
		if err != nil {
			return err
		}
		if rowsAffected(res) != 1 {
			return nil
		}
		if runID, err = endOrSynthesizeRunFn(ctx, conn, taskID, "blocked", "blocked", reason, nil, truthyStr(reason), runProfile{}); err != nil {
			return err
		}
		if err := appendEvent(ctx, conn, taskID, route.EventKind, route.Payload, runID); err != nil {
			return err
		}
		if blockedTask, err = GetTask(ctx, conn, taskID); err != nil {
			return err
		}
		blocked, transitioned = true, true
		return nil
	})
	if err != nil || !blocked {
		return false, err
	}
	if transitioned {
		fireTaskHook(ctx, "kanban_task_blocked", blockedTask, taskID, runID, map[string]any{"reason": optStrValue(reason)})
	}
	return true, nil
}

// Zero for a falsy cell like the or 0 idiom, the cell otherwise
func orZero(v any) any {
	if !truthy(v) {
		return int64(0)
	}
	return v
}

// blockRoute is the status, event kind, SQL fragment, parameters and payload of one block transition
type blockRoute struct {
	NewStatus string
	EventKind string
	SetSQL    string
	Params    []any
	Payload   *pyjson.Object
}

// Routes a block by kind, dependency waiting in todo and a repeated kind counting toward the triage limit
func routeBlock(kind, reason *string, sourceStatus string, prevKind *string, prevRecurrences int64) blockRoute {
	body := payload("reason", optStrValue(reason), "kind", optStrValue(kind), "source_status", sourceStatus)
	if kind != nil && *kind == "dependency" {
		return blockRoute{"todo", "dependency_wait", "block_kind    = ?", []any{*kind}, body}
	}
	recurrences := int64(1)
	if strPtrEq(prevKind, kind) {
		recurrences = prevRecurrences + 1
	}
	setSQL := "block_kind    = ?,\n                       block_recurrences = ?"
	body = payload("reason", optStrValue(reason), "kind", optStrValue(kind), "recurrences", recurrences, "source_status", sourceStatus)
	params := []any{optStrValue(kind), recurrences}
	if recurrences >= BlockRecurrenceLimit {
		body.Set("limit", int64(BlockRecurrenceLimit))
		return blockRoute{"triage", "block_loop_detected", setSQL, params, body}
	}
	return blockRoute{"blocked", "blocked", setSQL, params, body}
}

// Redacts secrets in strings, dicts and lists at the domain boundary for durable review handoffs
func redactReviewValue(value any) any {
	switch v := value.(type) {
	case string:
		return tools.RedactSensitiveTextWith(nil, v, tools.RedactOptions{Force: true})
	case *pyjson.Object:
		if v == nil {
			return v
		}
		out := pyjson.NewObject()
		for _, k := range v.Keys {
			item, _ := v.Get(k)
			out.Set(k, redactReviewValue(item))
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactReviewValue(item)
		}
		return out
	}
	return value
}

// Redacted copy of an optional string
func redactOptStr(s *string) *string {
	if s == nil {
		return nil
	}
	r := redactReviewValue(*s).(string)
	return &r
}

// Redacted copy of an optional metadata dict
func redactMeta(o *pyjson.Object) *pyjson.Object {
	if o == nil {
		return nil
	}
	return redactReviewValue(o).(*pyjson.Object)
}

// RequestReviewOptions holds the keyword arguments of a review handoff
type RequestReviewOptions struct {
	Summary       *string
	Metadata      *pyjson.Object
	Reviewer      *string
	ExpectedRunID *int64
	Force         bool
}

// Moves running or ready to review with implementer and reviewer provenance, staged artifacts rolled back on failure
func RequestReview(ctx context.Context, conn state.SQLiteConn, taskID string, o RequestReviewOptions) (bool, string, error) {
	summary := redactOptStr(o.Summary)
	metadata, err := mergeCompletionProseArtifacts(ctx, conn, taskID, redactMeta(o.Metadata), summary, nil)
	if err != nil {
		return false, "", err
	}
	now := time.Now().Unix()
	reviewer := o.Reviewer
	var stagedCopies []string
	refusal := ""
	err = WriteTxn(ctx, conn, false, func() error {
		satisfied, err := parentsSatisfied(ctx, conn, taskID)
		if err != nil {
			return err
		}
		if !satisfied {
			refusal = "parent dependencies are not satisfied"
			return nil
		}
		trow, err := QueryOne(ctx, conn, "SELECT assignee, status, claim_lock, current_run_id, worker_pid, "+
			"worker_started_at FROM tasks WHERE id = ?", taskID)
		if err != nil {
			return err
		}
		if trow == nil {
			refusal = "task not found"
			return nil
		}
		if o.ExpectedRunID == nil && !o.Force && claimIsLive(trow) {
			refusal = "task is running under a live claim; pass expected_run_id " +
				"(worker ownership) or force=True (explicit operator " +
				"override) instead of clearing the live run's claim"
			return nil
		}
		if reviewer == nil {
			prior, malformed, err := priorReviewer(ctx, conn, taskID)
			if err != nil {
				return err
			}
			if malformed {
				refusal = "re-review has no durable reviewer provenance (the " +
					"latest changes_requested event is missing or " +
					"malformed); pass reviewer= explicitly"
				return nil
			}
			reviewer = prior
		}
		if reviewer, err = canonicalAssignee(reviewer); err != nil {
			return err
		}
		var implementer any
		if trow.Get("current_run_id") != nil {
			arow, err := QueryOne(ctx, conn, "SELECT profile FROM task_runs WHERE id = ?", trow.Get("current_run_id"))
			if err != nil {
				return err
			}
			if arow != nil {
				implementer = arow.Get("profile")
			}
		}
		if implementer == nil && !jeq(trow.Get("assignee"), optStrValue(reviewer)) {
			implementer = trow.Get("assignee")
		}
		assigneeSQL, runGuard := "", ""
		var params []any
		if reviewer != nil {
			assigneeSQL = ", assignee = ?"
			params = append(params, *reviewer)
		}
		params = append(params, taskID)
		if o.ExpectedRunID != nil {
			if err := runIDBindable(o.ExpectedRunID); err != nil {
				return err
			}
			runGuard = " AND current_run_id = ?"
			params = append(params, *o.ExpectedRunID)
		}
		res, err := conn.ExecContext(ctx, `
                UPDATE tasks
                   SET status        = 'review',
                       claim_lock    = NULL,
                       claim_expires = NULL,
                       worker_pid    = NULL
                `+assigneeSQL+`
                 WHERE id = ?
                   AND status IN ('running', 'ready')
                `+runGuard, params...)
		if err != nil {
			return err
		}
		if rowsAffected(res) != 1 {
			refusal = "task is not in running/ready (or expected_run_id did not match the current run)"
			return nil
		}
		if metadata != nil {
			if stagedCopies, err = stageCompletionArtifacts(ctx, conn, taskID, metadata, now, "kanban_request_review"); err != nil {
				return err
			}
		}
		runID, err := endOrSynthesizeRunFn(ctx, conn, taskID, "review_requested", "review", summary, metaAny(metadata),
			truthyStr(summary) || truthyMeta(metadata), runProfile{Set: true, Value: cellOptStr(implementer)})
		if err != nil {
			return err
		}
		body := payload("summary", firstLineOrNil(summary, 400), "implementer", implementer, "reviewer", optStrValue(reviewer))
		if staged := cleanedArtifactPaths(metaAny(metadata)); len(staged) > 0 {
			body.Set("artifacts", staged)
		}
		return appendEvent(ctx, conn, taskID, "review_requested", body, runID)
	})
	if err != nil {
		if len(stagedCopies) > 0 {
			discardStagedCopyList(stagedCopies, pypath.PathParent(stagedCopies[0]))
		}
		return false, "", err
	}
	if refusal != "" {
		return false, refusal, nil
	}
	return true, "", nil
}

// Reviewer recorded by the latest changes_requested run, nil for a first review and malformed when the provenance is missing
func priorReviewer(ctx context.Context, conn state.SQLiteConn, taskID string) (*string, bool, error) {
	changesRun, err := QueryOne(ctx, conn, "SELECT id FROM task_runs "+
		"WHERE task_id = ? AND outcome = 'changes_requested' "+
		"ORDER BY id DESC LIMIT 1", taskID)
	if err != nil || changesRun == nil {
		return nil, false, err
	}
	runID, err := pyIntOf(changesRun.Get("id"))
	if err != nil {
		return nil, false, err
	}
	changesEvent, err := latestEvent(ctx, conn, taskID, "changes_requested", &runID)
	if err != nil {
		return nil, false, err
	}
	reviewer, _ := jsonDict(rowGet(changesEvent, "payload", nil)).Get("reviewer")
	if s, ok := reviewer.(string); ok && pystr.Strip(s) != "" {
		return &s, false, nil
	}
	return nil, true, nil
}

// The value when it is a string with non whitespace content, nil otherwise
func nonblankStr(value any) *string {
	if s, ok := value.(string); ok && pystr.Strip(s) != "" {
		return &s
	}
	return nil
}

// Closes an active reviewer run and hands the task back to the implementer of the latest review request
func RequestChanges(ctx context.Context, conn state.SQLiteConn, taskID, reason string, expectedRunID *int64) (bool, string, error) {
	reason = pystr.Strip(redactReviewValue(reason).(string))
	if reason == "" {
		return false, "reason is required", nil
	}
	refusal, implementer := "", ""
	err := WriteTxn(ctx, conn, false, func() error {
		taskRow, err := QueryOne(ctx, conn, "SELECT status, assignee, current_run_id FROM tasks WHERE id = ?", taskID)
		if err != nil {
			return err
		}
		if taskRow == nil {
			refusal = "task not found"
			return nil
		}
		currentRunID := taskRow.Get("current_run_id")
		if taskRow.Get("status") != "running" || currentRunID == nil {
			refusal = "task is not in an active review run"
			return nil
		}
		runID, err := pyIntOf(currentRunID)
		if err != nil {
			return err
		}
		if expectedRunID != nil && runID != *expectedRunID {
			refusal = "run_id mismatch"
			return nil
		}
		claimedEvent, err := latestEvent(ctx, conn, taskID, "claimed", &runID)
		if err != nil {
			return err
		}
		if source, _ := jsonDict(rowGet(claimedEvent, "payload", nil)).Get("source_status"); !jeq(source, "review") {
			refusal = "active run was not claimed from review"
			return nil
		}
		requestedEvent, err := latestEvent(ctx, conn, taskID, "review_requested", nil)
		if err != nil {
			return err
		}
		if requestedEvent == nil {
			refusal = "no prior review_requested event"
			return nil
		}
		impl, _ := jsonDict(requestedEvent.Get("payload")).Get("implementer")
		implPtr := nonblankStr(impl)
		if implPtr == nil {
			refusal = "review handoff has no valid implementer provenance"
			return nil
		}
		reviewer, err := canonicalAssignee(nonblankStr(taskRow.Get("assignee")))
		if err != nil {
			return err
		}
		newStatus, err := landingStatusAfterParents(ctx, conn, taskID)
		if err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, `
            UPDATE tasks
               SET status = ?,
                   assignee = COALESCE(?, assignee),
                   claim_lock = NULL,
                   claim_expires = NULL,
                   worker_pid = NULL, worker_started_at = NULL
             WHERE id = ? AND status = 'running' AND current_run_id = ?
            `, newStatus, *implPtr, taskID, runID)
		if err != nil {
			return err
		}
		if rowsAffected(res) != 1 {
			refusal = "task changed during review handoff"
			return nil
		}
		endedRun, err := endRun(ctx, conn, taskID, "changes_requested", &reason, nil, nil, newStatus)
		if err != nil {
			return err
		}
		implementer = *implPtr
		return appendEvent(ctx, conn, taskID, "changes_requested",
			payload("reason", reason, "implementer", *implPtr, "reviewer", optStrValue(reviewer), "status", newStatus), endedRun)
	})
	if err != nil {
		return false, "", err
	}
	if refusal != "" {
		return false, refusal, nil
	}
	return true, implementer, nil
}

// Operator promotion of todo or blocked to ready with an audit event, refused while a parent is unfinished
func PromoteTask(ctx context.Context, conn state.SQLiteConn, taskID, actor string, reason *string, dryRun bool) (bool, string, error) {
	curStatus, err := taskStatusOf(ctx, conn, taskID)
	if err != nil {
		return false, "", err
	}
	if curStatus == nil {
		return false, fmt.Sprintf("task %s not found", taskID), nil
	}
	if *curStatus != "todo" && *curStatus != "blocked" {
		return false, fmt.Sprintf("task %s is %s; promote only applies to 'todo' or 'blocked'", taskID, pyrepr.Repr(*curStatus)), nil
	}
	parents, err := QueryRows(ctx, conn, "SELECT t.id, t.status FROM tasks t "+
		"JOIN task_links l ON l.parent_id = t.id "+
		"WHERE l.child_id = ?", taskID)
	if err != nil {
		return false, "", err
	}
	var unsatisfied []string
	for _, p := range parents {
		if st := p.Get("status"); st != "done" && st != "archived" {
			unsatisfied = append(unsatisfied, cellStr(p.Get("id")))
		}
	}
	if len(unsatisfied) > 0 {
		return false, fmt.Sprintf("unsatisfied parent dependencies: %s "+
			"(the ready -> running claim re-checks parents, so promotion cannot "+
			"bypass them; complete the parents or drop the link with "+
			"`icebath kanban unlink <parent_id> %s`)", strings.Join(unsatisfied, ", "), taskID), nil
	}
	if dryRun {
		return true, "", nil
	}
	refusal := ""
	err = WriteTxn(ctx, conn, false, func() error {
		res, err := conn.ExecContext(ctx, "UPDATE tasks SET status = 'ready' "+
			"WHERE id = ? AND status IN ('todo', 'blocked')", taskID)
		if err != nil {
			return err
		}
		if rowsAffected(res) != 1 {
			refusal = fmt.Sprintf("task %s status changed during promotion", taskID)
			return nil
		}
		return appendEvent(ctx, conn, taskID, "promoted_manual", payload("actor", actor, "reason", optStrValue(reason)), nil)
	})
	if err != nil {
		return false, "", err
	}
	if refusal != "" {
		return false, refusal, nil
	}
	return true, "", nil
}

// Ready when every parent is terminal else todo, the re gate unblock and reopen share
func landingStatusAfterParents(ctx context.Context, conn state.SQLiteConn, taskID string) (string, error) {
	satisfied, err := parentsSatisfied(ctx, conn, taskID)
	if err != nil {
		return "", err
	}
	if satisfied {
		return "ready", nil
	}
	return "todo", nil
}

// Moves blocked or scheduled back to its resumable phase after re gating on parents, closing any leaked run first
func UnblockTask(ctx context.Context, conn state.SQLiteConn, taskID string) (bool, error) {
	now := time.Now().Unix()
	unblocked := false
	err := WriteTxn(ctx, conn, false, func() error {
		status, err := taskStatusOf(ctx, conn, taskID)
		if err != nil {
			return err
		}
		resumeStatus := "ready"
		if status != nil && *status == "blocked" {
			if resumeStatus, err = resumeStatusFromEvents(ctx, conn, taskID); err != nil {
				return err
			}
		}
		if err := reclaimDanglingRun(ctx, conn, taskID, []string{"blocked", "scheduled"}, now, "invariant recovery on unblock"); err != nil {
			return err
		}
		landing, err := landingStatusAfterParents(ctx, conn, taskID)
		if err != nil {
			return err
		}
		newStatus := landing
		if landing == "ready" && resumeStatus == "review" {
			newStatus = "review"
		}
		res, err := conn.ExecContext(ctx, "UPDATE tasks SET status = ?, current_run_id = NULL, "+
			"consecutive_failures = 0, last_failure_error = NULL "+
			"WHERE id = ? AND status IN ('blocked', 'scheduled')", newStatus, taskID)
		if err != nil {
			return err
		}
		if rowsAffected(res) != 1 {
			return nil
		}
		var body any
		if newStatus != "ready" || resumeStatus != "ready" {
			body = payload("status", newStatus, "resume_status", resumeStatus)
		}
		unblocked = true
		return appendEvent(ctx, conn, taskID, "unblocked", body, nil)
	})
	return unblocked && err == nil, err
}

// Moves review back to ready or todo restoring the implementer from the review request
func ReopenReviewTask(ctx context.Context, conn state.SQLiteConn, taskID string) (bool, error) {
	now := time.Now().Unix()
	reopened := false
	err := WriteTxn(ctx, conn, false, func() error {
		if err := reclaimDanglingRun(ctx, conn, taskID, []string{"review"}, now, "invariant recovery on review reopen"); err != nil {
			return err
		}
		newStatus, err := landingStatusAfterParents(ctx, conn, taskID)
		if err != nil {
			return err
		}
		reviewEvent, err := latestEvent(ctx, conn, taskID, "review_requested", nil)
		if err != nil {
			return err
		}
		impl, _ := jsonDict(rowGet(reviewEvent, "payload", nil)).Get("implementer")
		implementer := nonblankStr(impl)
		params := []any{newStatus}
		assigneeSQL := ""
		if implementer != nil {
			params = append(params, *implementer)
			assigneeSQL = ", assignee = ?"
		}
		params = append(params, taskID)
		res, err := conn.ExecContext(ctx, "UPDATE tasks SET status = ?, current_run_id = NULL, "+
			"claim_lock = NULL, claim_expires = NULL, worker_pid = NULL, worker_started_at = NULL "+
			assigneeSQL+" WHERE id = ? AND status = 'review'", params...)
		if err != nil {
			return err
		}
		if rowsAffected(res) != 1 {
			return nil
		}
		var body any
		if implementer != nil {
			body = payload("status", newStatus, "implementer", *implementer)
		} else if newStatus != "ready" {
			body = payload("status", newStatus)
		}
		reopened = true
		return appendEvent(ctx, conn, taskID, "review_reopened", body, nil)
	})
	return reopened && err == nil, err
}

// WorkerTermination is the pid, claim lock and start time of a worker to signal after commit
type WorkerTermination struct {
	Pid       any
	ClaimLock *string
	StartedAt any
}

// InvalidationResult lists every demoted descendant and the workers to terminate post commit
type InvalidationResult struct {
	Invalidated  []*pyjson.Object
	Terminations []WorkerTermination
}

// Demotes every ready, review, running or done descendant of a reopened ancestor to todo with its audit trail
func InvalidateDescendantsForParentReopen(ctx context.Context, conn state.SQLiteConn, taskID, author string) (*InvalidationResult, error) {
	callerOwnsTxn := inTransaction(conn)
	now := time.Now().Unix()
	result := &InvalidationResult{}
	err := WriteTxn(ctx, conn, true, func() error {
		rows, err := QueryRows(ctx, conn, `
            WITH RECURSIVE descendants(id) AS (
                SELECT child_id FROM task_links WHERE parent_id = ?
                UNION
                SELECT l.child_id
                FROM task_links l
                JOIN descendants d ON d.id = l.parent_id
            )
            SELECT t.id, t.status, t.current_run_id, t.worker_pid, t.claim_lock, t.worker_started_at
            FROM descendants d
            JOIN tasks t ON t.id = d.id
            ORDER BY t.id
            `, taskID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			previousStatus := cellStr(row.Get("status"))
			if previousStatus != "ready" && previousStatus != "review" && previousStatus != "running" && previousStatus != "done" {
				continue
			}
			id := cellStr(row.Get("id"))
			resumeStatus := "ready"
			var runID *int64
			if previousStatus == "review" {
				resumeStatus = "review"
			} else if previousStatus == "running" {
				if resumeStatus, err = retryStatusForRun(ctx, conn, id, cellOptInt(row.Get("current_run_id"))); err != nil {
					return err
				}
				result.Terminations = append(result.Terminations, WorkerTermination{row.Get("worker_pid"), cellOptStr(row.Get("claim_lock")), row.Get("worker_started_at")})
				summary := fmt.Sprintf("ancestor %s reopened", taskID)
				if runID, err = endRun(ctx, conn, id, "reclaimed", &summary, nil, nil, "todo"); err != nil {
					return err
				}
			}
			if _, err := conn.ExecContext(ctx, "UPDATE tasks SET status = 'todo', completed_at = NULL, "+
				"claim_lock = NULL, claim_expires = NULL, worker_pid = NULL, worker_started_at = NULL, "+
				"current_run_id = NULL, consecutive_failures = 0 WHERE id = ?", id); err != nil {
				return err
			}
			entry := payload("id", id, "prior_status", previousStatus, "new_status", "todo", "resume_status", resumeStatus)
			if err := appendEvent(ctx, conn, id, "descendant_invalidated",
				payload("ancestor", taskID, "prior_status", previousStatus, "new_status", "todo", "resume_status", resumeStatus), runID); err != nil {
				return err
			}
			if err := appendEvent(ctx, conn, id, "status",
				payload("status", "todo", "reason", "ancestor_reopened", "parent", taskID, "previous_status", previousStatus, "resume_status", resumeStatus), runID); err != nil {
				return err
			}
			if err := insertComment(ctx, conn, id, author, fmt.Sprintf("Invalidated: ancestor %s was reopened; "+
				"retracted from '%s' to 'todo' "+
				"(will resume via '%s').", taskID, previousStatus, resumeStatus), now); err != nil {
				return err
			}
			result.Invalidated = append(result.Invalidated, entry)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !callerOwnsTxn {
		for _, t := range result.Terminations {
			terminateReclaimedWorkerFn(t.Pid, t.ClaimLock, nil, t.StartedAt)
		}
	}
	return result, nil
}

// Updates title, body and assignee when given and moves triage to todo in one transaction
func SpecifyTriageTask(ctx context.Context, conn state.SQLiteConn, taskID string, title, body, assignee, author *string) (bool, error) {
	if title != nil && pystr.Strip(*title) == "" {
		return false, &ValueError{"title cannot be blank"}
	}
	assignee, err := canonicalAssignee(assignee)
	if err != nil {
		return false, err
	}
	specified := false
	err = WriteTxn(ctx, conn, false, func() error {
		existing, err := QueryOne(ctx, conn, "SELECT title, body, assignee FROM tasks WHERE id = ? AND status = 'triage'", taskID)
		if err != nil || existing == nil {
			return err
		}
		sets := []string{"status = 'todo'"}
		var params []any
		var changedFields []string
		if title != nil && pystr.Strip(*title) != cellStr(existing.Get("title")) {
			sets = append(sets, "title = ?")
			params = append(params, pystr.Strip(*title))
			changedFields = append(changedFields, "title")
		}
		if body != nil && *body != cellStr(existing.Get("body")) {
			sets = append(sets, "body = ?")
			params = append(params, *body)
			changedFields = append(changedFields, "body")
		}
		if assignee != nil && !strPtrEq(assignee, cellOptStr(orNil(existing.Get("assignee")))) {
			sets = append(sets, "assignee = ?")
			params = append(params, *assignee)
			changedFields = append(changedFields, "assignee")
		}
		params = append(params, taskID)
		res, err := conn.ExecContext(ctx, "UPDATE tasks SET "+strings.Join(sets, ", ")+" WHERE id = ? AND status = 'triage'", params...)
		if err != nil {
			return err
		}
		if rowsAffected(res) != 1 {
			return nil
		}
		if len(changedFields) > 0 && author != nil && pystr.Strip(*author) != "" {
			if err := insertComment(ctx, conn, taskID, pystr.Strip(*author),
				"Specified — updated "+strings.Join(changedFields, ", ")+" and promoted to todo.", time.Now().Unix()); err != nil {
				return err
			}
		}
		var evBody any
		if len(changedFields) > 0 {
			evBody = payload("changed_fields", changedFields)
		}
		specified = true
		return appendEvent(ctx, conn, taskID, "specified", evBody, nil)
	})
	if err != nil || !specified {
		return false, err
	}
	if _, err := RecomputeReady(ctx, conn, nil); err != nil {
		return false, err
	}
	return true, nil
}
