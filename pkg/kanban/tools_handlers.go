package kanban

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

var (
	sqliteRenderOnce sync.Once
	sqliteRenderDB   *sql.DB
	sqliteRenderErr  error
)

// Text the bundled SQLite stores in a TEXT column for a REAL, nil for NaN which binds as NULL
func sqliteRealText(v float64) (*string, error) {
	sqliteRenderOnce.Do(func() { sqliteRenderDB, sqliteRenderErr = sql.Open("sqlite", ":memory:") })
	if sqliteRenderErr != nil {
		return nil, sqliteRenderErr
	}
	var text sql.NullString
	if err := sqliteRenderDB.QueryRow("SELECT CAST(? AS TEXT)", v).Scan(&text); err != nil {
		return nil, err
	}
	if !text.Valid {
		return nil, nil
	}
	return &text.String, nil
}

// Text SQLite stores in a TEXT column for a bound Python value, the sqlite3 binding error at that position otherwise
func sqliteBind(value any, position int) (*string, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		return &v, nil
	case bool:
		text := "0"
		if v {
			text = "1"
		}
		return &text, nil
	case float64:
		return sqliteRealText(v)
	case float32:
		return sqliteRealText(float64(v))
	}
	if n, isInt := pyrepr.Int(value); isInt {
		if !n.IsInt64() {
			return nil, &RuntimeError{"Python int too large to convert to SQLite INTEGER"}
		}
		text := n.String()
		return &text, nil
	}
	return nil, &RuntimeError{fmt.Sprintf("Error binding parameter %d: type '%s' is not supported", position, pyrepr.TypeName(value))}
}

// Whether a Python value can be a set element, false for the list and dict shapes JSON decodes to
func pyHashable(value any) bool {
	switch value.(type) {
	case []any, []string, *pyjson.Object, map[string]any:
		return false
	}
	return true
}

// TypeError Python raises testing an unhashable value for set membership
func unhashableSetError(value any) error {
	name := pyrepr.TypeName(value)
	return &RuntimeError{fmt.Sprintf("cannot use '%s' as a set element (unhashable type: '%s')", name, name)}
}

// Full task state, the row with parents, children, comments, runs and its last fifty events
func handleShow(k *kanbanCall) (string, error) {
	tid, ok := defaultTaskID(k, k.get("task_id"))
	if !ok {
		return pyjson.Dumps(pyjson.ObjectOf("current_task", nil,
			"hint", "No dispatcher-assigned task in this session, so there is no "+
				"task to show by default. Call kanban_list to see task ids on "+
				"the board, then kanban_show(task_id=<id>) for full state."))
	}
	return withBoard(k.ctx, k.get("board"), false, func(conn state.SQLiteConn) (string, error) {
		task, err := existingTask(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		parents, err := ParentIDs(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		open, err := UnsatisfiedParents(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		unsatisfied := make([]any, 0, len(open))
		for _, p := range open {
			unsatisfied = append(unsatisfied, pyjson.ObjectOf("id", p.ID, "status", p.Status))
		}
		children, err := ChildIDs(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		commentRows, err := ListComments(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		comments := make([]any, 0, len(commentRows))
		for _, c := range commentRows {
			comments = append(comments, fieldsOf(c, commentFields))
		}
		eventRows, err := ListEvents(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		if len(eventRows) > 50 {
			eventRows = eventRows[len(eventRows)-50:]
		}
		events := make([]any, 0, len(eventRows))
		for _, e := range eventRows {
			events = append(events, fieldsOf(e, eventFields))
		}
		runRows, err := ListRuns(k.ctx, conn, tid, true, nil, nil)
		if err != nil {
			return "", err
		}
		runs := make([]any, 0, len(runRows))
		for _, r := range runRows {
			runs = append(runs, fieldsOf(r, runFields))
		}
		workerContext, err := BuildWorkerContext(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		return pyjson.Dumps(pyjson.ObjectOf("task", fieldsOf(task, taskFields), "parents", idList(parents),
			"unsatisfied_parents", unsatisfied, "children", idList(children), "comments", comments,
			"events", events, "runs", runs, "worker_context", workerContext))
	})
}

// Task summaries with the core filters of the CLI listing, one extra row read to report truncation
func handleList(k *kanbanCall) (string, error) {
	if err := requireOrchestratorTool("kanban_list"); err != nil {
		return "", err
	}
	includeArchived, err := parseBoolArg(k, "include_archived")
	if err != nil {
		return "", err
	}
	limit := int64(KanbanListDefaultLimit)
	if raw := k.get("limit"); raw != nil {
		n, err := pyint.Of(raw)
		if err != nil {
			var pe *pyint.Error
			if errors.As(err, &pe) && (pe.Kind == "TypeError" || pe.Kind == "ValueError") {
				return kanbanToolError("limit must be an integer"), nil
			}
			return "", err
		}
		switch {
		case n.Sign() < 1:
			return "", reject("limit must be >= 1")
		case !n.IsInt64() || n.Int64() > KanbanListMaxLimit:
			return "", reject(fmt.Sprintf("limit must be <= %d", KanbanListMaxLimit))
		}
		limit = n.Int64()
	}
	return withBoard(k.ctx, k.get("board"), false, func(conn state.SQLiteConn) (string, error) {
		promoted, err := RecomputeReady(k.ctx, conn, nil)
		if err != nil {
			return "", err
		}
		o := ListTasksOptions{IncludeArchived: includeArchived, Limit: limit + 1}
		switch raw := k.get("status").(type) {
		case nil:
		case string:
			o.Status = &raw
		default:
			if !pyHashable(raw) {
				return "", unhashableSetError(raw)
			}
			return "", &ValueError{fmt.Sprintf("status must be one of %s", sortedRepr(ValidStatuses))}
		}
		if o.Status != nil && !ValidStatuses[*o.Status] {
			return "", &ValueError{fmt.Sprintf("status must be one of %s", sortedRepr(ValidStatuses))}
		}
		position := 1
		if raw := k.get("assignee"); raw != nil {
			name, err := profiles.NormalizeProfileNameValue(raw)
			if err != nil {
				return "", &ValueError{err.Error()}
			}
			o.Assignee = &name
			position++
		}
		if o.Status != nil {
			position++
		}
		if raw := k.get("tenant"); raw != nil {
			if o.Tenant, err = sqliteBind(raw, position); err != nil {
				return "", err
			}
		}
		rows, err := ListTasks(k.ctx, conn, o)
		if err != nil {
			return "", err
		}
		truncated := int64(len(rows)) > limit
		if truncated {
			rows = rows[:limit]
		}
		tasks := make([]any, 0, len(rows))
		for _, t := range rows {
			summary, err := taskSummaryDict(k.ctx, conn, t)
			if err != nil {
				return "", err
			}
			tasks = append(tasks, summary)
		}
		var nextLimit any
		if truncated && limit < KanbanListMaxLimit {
			nextLimit = min(limit*2, int64(KanbanListMaxLimit))
		}
		return pyjson.Dumps(pyjson.ObjectOf("tasks", tasks, "count", int64(len(tasks)), "limit", limit,
			"truncated", truncated, "next_limit", nextLimit, "promoted", int64(promoted)))
	})
}

// Marks the current task done with a structured handoff and reports the attachments the completion registered
func handleComplete(k *kanbanCall) (string, error) {
	tid, err := workerGuard(k, "kanban_complete")
	if err != nil {
		return "", err
	}
	summary, result := redactOpt(k.env, k.get("summary")), redactOpt(k.env, k.get("result"))
	metadata := k.get("metadata")
	if dict, isDict := dictObject(metadata); isDict {
		metadata = dict
		redacted, err := redactMetadata(k.env, dict)
		if err != nil {
			return "", err
		}
		if redacted != nil && redacted.Len() > 0 {
			metadata = redacted
		}
	}
	createdCards, hasCards, err := coerceStrList(k.get("created_cards"), "created_cards", "task ids", true)
	if err != nil {
		return "", err
	}
	artifacts, _, err := coerceStrList(k.get("artifacts"), "artifacts", "file paths", true)
	if err != nil {
		return "", err
	}
	if len(artifacts) > 0 {
		merged, err := mergeArtifacts(metadata, artifacts)
		if err != nil {
			return "", err
		}
		metadata = merged
	}
	if !pyrepr.Truthy(summary) && !pyrepr.Truthy(result) {
		return "", reject("provide at least one of: summary (preferred), result")
	}
	if err := requireDictMetadata(metadata); err != nil {
		return "", err
	}
	meta, _ := metadata.(*pyjson.Object)
	meta = stampWorkerSessionMetadata(tid, meta)
	return withBoard(k.ctx, k.get("board"), false, func(conn state.SQLiteConn) (string, error) {
		task, err := GetTask(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		evidence := summary
		if !pyrepr.Truthy(evidence) {
			evidence = result
		}
		if err := goalGate(k, "kanban_complete", task, tid, pystr.Strip(pyrepr.Str(evidence))); err != nil {
			return "", err
		}
		o := CompleteTaskOptions{Metadata: meta, ExpectedRunID: expectedRunID(tid)}
		if hasCards {
			o.CreatedCards = append([]string{}, strItems(createdCards)...)
		}
		o.Result, _ = sqliteBind(result, 1)
		o.Summary, _ = sqliteBind(summary, 3)
		if _, isText := result.(string); result != nil && !isText {
			o.resultRaw = result
		}
		if _, isText := summary.(string); summary != nil && !isText {
			o.summaryRaw = summary
		}
		ok, err := CompleteTask(k.ctx, conn, tid, o)
		if err != nil {
			var artifactErr *ArtifactPreservationError
			var claimErr *LiveClaimError
			var cardsErr *HallucinatedCardsError
			var emptyErr *EmptyCompletionError
			switch {
			case errors.As(err, &artifactErr):
				return kanbanToolError(fmt.Sprintf("kanban_complete could not preserve the declared artifacts: %s. "+
					"Your task is still in-flight and its scratch workspace was kept. Fix the "+
					"artifact path or storage error, then retry kanban_complete with the same "+
					"handoff.", artifactErr.Error())), nil
			case errors.As(err, &claimErr):
				return kanbanToolError(fmt.Sprintf("kanban_complete refused: %s. Nothing changed. Wait for the worker "+
					"to finish, or an operator can run `icebath kanban complete --force %s`.", claimErr.Error(), tid)), nil
			case errors.As(err, &cardsErr):
				return kanbanToolError(fmt.Sprintf("kanban_complete blocked: the following created_cards do not exist or were not "+
					"created by this worker: %s. Your task is still "+
					"in-flight (no state change). Retry kanban_complete with the same "+
					"summary/metadata and either drop these ids from created_cards, or pass "+
					"created_cards=[] to skip the card-claim check entirely.", strings.Join(cardsErr.Phantom, ", "))), nil
			case errors.As(err, &emptyErr):
				return kanbanToolError(fmt.Sprintf("kanban_complete blocked: %s. Your task is still in-flight (no state "+
					"change). Retry kanban_complete with a non-empty summary or result describing "+
					"what was done.", emptyErr.Error())), nil
			}
			return "", err
		}
		if task, err = GetTask(k.ctx, conn, tid); err != nil {
			return "", err
		}
		if !ok {
			blockers, err := UnsatisfiedParents(k.ctx, conn, tid)
			if err != nil {
				return "", err
			}
			if len(blockers) > 0 {
				parts := make([]string, 0, len(blockers))
				for _, b := range blockers {
					parts = append(parts, fmt.Sprintf("%s (%s)", b.ID, b.Status))
				}
				return "", reject(fmt.Sprintf("could not complete %s: unsatisfied parent dependencies: "+
					"%s; complete the parents first (done or archived)", tid, strings.Join(parts, ", ")))
			}
			if task != nil && truthyStr(task.LastFailureError) {
				return "", reject(*task.LastFailureError)
			}
			return "", reject(fmt.Sprintf("could not complete %s (unknown id, stale run, or already terminal)", tid))
		}
		run, err := LatestRun(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		var runID any
		if run != nil {
			runID = run.ID
		}
		stored, err := ListAttachments(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		attachments := make([]any, 0, len(stored))
		for _, a := range stored {
			attachments = append(attachments, fieldsOf(a, attachmentFields))
		}
		return okPayload("task_id", tid, "run_id", runID, "attachments", attachments)
	})
}

// Moves the task to blocked with a reason a human reads, goal mode tasks only for external blockers
func handleBlock(k *kanbanCall) (string, error) {
	tid, err := workerGuard(k, "kanban_block")
	if err != nil {
		return "", err
	}
	rawReason, err := requireText(k, "reason", "reason is required — explain what input you need")
	if err != nil {
		return "", err
	}
	reason := redactText(k.env, rawReason)
	kind := k.get("kind")
	return withBoard(k.ctx, k.get("board"), false, func(conn state.SQLiteConn) (string, error) {
		kindText, isText := kind.(string)
		if kind != nil {
			if !pyHashable(kind) {
				return "", unhashableSetError(kind)
			}
			if !isText || !ValidBlockKinds[kindText] {
				return "", reject(fmt.Sprintf("kind must be one of %s (or omit it)", sortedRepr(ValidBlockKinds)))
			}
		}
		task, err := GetTask(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		if task != nil && task.GoalMode && !(isText && goalModeBlockAllowedKinds[kindText]) {
			return "", reject(fmt.Sprintf("goal_mode tasks can only block with kind in "+
				"%s (got %s). If the task is actually "+
				"finished or cannot proceed for another reason, call kanban_complete instead — "+
				"the completion judge will evaluate it.", sortedRepr(goalModeBlockAllowedKinds), pyrepr.Repr(kind)))
		}
		var kindArg *string
		if isText {
			kindArg = &kindText
		}
		ok, err := BlockTaskWithKind(k.ctx, conn, tid, &reason, kindArg, expectedRunID(tid))
		if err != nil {
			return "", err
		}
		if !ok {
			return "", reject(fmt.Sprintf("could not block %s (unknown id or not in running/ready)", tid))
		}
		landed, err := GetTask(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		if landed == nil {
			return "", &RuntimeError{"'NoneType' object has no attribute 'block_kind'"}
		}
		extra := []any{"block_kind", optStrValue(landed.BlockKind)}
		if isText && kindText == "dependency" && (landed.BlockKind == nil || *landed.BlockKind != kindText) {
			extra = append(extra, "requested_kind", kindText, "note",
				"kind='dependency' only waits on an incomplete parent; no parent is open, "+
					"so this was recorded as needs_input (sticky until a human unblocks) "+
					"instead of parking in todo where the dispatcher would respawn it.")
		}
		return okLanded(k.ctx, conn, tid, "blocked", extra...)
	})
}

// Parks the current task as scheduled until an orchestrator gates it again
func handleSchedule(k *kanbanCall) (string, error) {
	tid, err := workerGuard(k, "kanban_schedule")
	if err != nil {
		return "", err
	}
	rawReason := k.get("reason")
	text, isText := rawReason.(string)
	if rawReason != nil && !isText {
		return "", reject("reason must be a string")
	}
	var reason *string
	if stripped := pystr.Strip(text); stripped != "" {
		redacted := redactText(k.env, stripped)
		reason = &redacted
	}
	return withBoard(k.ctx, k.get("board"), false, func(conn state.SQLiteConn) (string, error) {
		task, err := GetTask(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		if task != nil && task.GoalMode {
			return "", reject("goal_mode tasks cannot be scheduled: use kanban_block with kind " +
				fmt.Sprintf("in %s for a genuine external ", sortedRepr(goalModeBlockAllowedKinds)) +
				"blocker, or kanban_complete so the completion judge can evaluate it.")
		}
		ok, err := ScheduleTask(k.ctx, conn, tid, reason, expectedRunID(tid))
		if err != nil {
			return "", err
		}
		if !ok {
			return "", reject(fmt.Sprintf("could not schedule %s (unknown id or not in todo/ready/running/blocked)", tid))
		}
		return okLanded(k.ctx, conn, tid, "scheduled", "reason", optStrValue(reason))
	})
}

// Hands a finished implementation to the review phase, the reviewer being an installed profile when named
func handleRequestReview(k *kanbanCall) (string, error) {
	tid, err := workerGuard(k, "kanban_request_review")
	if err != nil {
		return "", err
	}
	rawSummary, err := requireText(k, "summary", "summary is required — describe what was implemented and how it "+
		"was verified so the reviewer has context")
	if err != nil {
		return "", err
	}
	summary := redactText(k.env, rawSummary)
	rawMetadata := k.get("metadata")
	if err := requireDictMetadata(rawMetadata); err != nil {
		return "", err
	}
	var metadata *pyjson.Object
	if rawMetadata != nil {
		dict, _ := dictObject(rawMetadata)
		if metadata, err = redactMetadata(k.env, dict); err != nil {
			return "", err
		}
		if metadata == nil {
			return "", reject("metadata could not be safely serialized")
		}
	}
	artifacts, _, err := coerceStrList(k.get("artifacts"), "artifacts", "file paths", true)
	if err != nil {
		return "", err
	}
	if len(artifacts) > 0 {
		var current any
		if metadata != nil {
			current = metadata
		}
		if metadata, err = mergeArtifacts(current, artifacts); err != nil {
			return "", err
		}
	}
	metadata = stampWorkerSessionMetadata(tid, metadata)
	var reviewer *string
	if raw := k.get("reviewer"); pyrepr.Truthy(raw) {
		name := redactText(k.env, raw)
		if name != "" {
			if !profileExistsFn(name) {
				return "", reject(fmt.Sprintf("reviewer profile %s is not installed. "+
					"Installed profiles: %s", pyrepr.StrRepr(name), strings.Join(profiles.ListProfileNames(), ", ")))
			}
		}
		reviewer = &name
	}
	return withBoard(k.ctx, k.get("board"), false, func(conn state.SQLiteConn) (string, error) {
		task, err := GetTask(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		if err := goalGate(k, "kanban_request_review", task, tid, summary); err != nil {
			return "", err
		}
		ok, failReason, err := RequestReview(k.ctx, conn, tid, RequestReviewOptions{Summary: &summary, Metadata: metadata,
			Reviewer: reviewer, ExpectedRunID: expectedRunID(tid)})
		if err != nil {
			var artifactErr *ArtifactPreservationError
			if errors.As(err, &artifactErr) {
				return kanbanToolError(fmt.Sprintf("kanban_request_review could not preserve the declared artifacts: %s. "+
					"Your task is still in-flight (no state change) and its scratch workspace was "+
					"kept. Fix the artifact path or storage error, then retry "+
					"kanban_request_review with the same handoff.", artifactErr.Error())), nil
			}
			return "", err
		}
		if !ok {
			if failReason == "" {
				failReason = "unknown id or not in running/ready"
			}
			return "", reject(fmt.Sprintf("could not request review for %s: %s", tid, failReason))
		}
		return okLanded(k.ctx, conn, tid, "review")
	})
}

// Returns a reviewer owned running task to its implementer with the changes it needs
func handleRequestChanges(k *kanbanCall) (string, error) {
	tid, err := workerGuard(k, "kanban_request_changes")
	if err != nil {
		return "", err
	}
	rawReason, err := requireText(k, "reason", "reason is required — describe the changes needed")
	if err != nil {
		return "", err
	}
	reason := redactText(k.env, rawReason)
	return withBoard(k.ctx, k.get("board"), false, func(conn state.SQLiteConn) (string, error) {
		ok, detail, err := RequestChanges(k.ctx, conn, tid, reason, expectedRunID(tid))
		if err != nil {
			return "", err
		}
		if !ok {
			if detail == "" {
				detail = "invalid review state"
			}
			return "", reject(fmt.Sprintf("could not request changes for %s: %s", tid, detail))
		}
		var implementer any
		if detail != "" {
			implementer = detail
		}
		return okLanded(k.ctx, conn, tid, "ready", "implementer", implementer)
	})
}

// Extends the claim and records a heartbeat event, the liveness signal of a long operation
func handleHeartbeat(k *kanbanCall) (string, error) {
	tid, err := workerGuard(k, "kanban_heartbeat")
	if err != nil {
		return "", err
	}
	return withBoard(k.ctx, k.get("board"), false, func(conn state.SQLiteConn) (string, error) {
		var claimer *string
		if lock, set := os.LookupEnv("ICEBATH_KANBAN_CLAIM_LOCK"); set {
			claimer = &lock
		}
		if _, err := HeartbeatClaim(k.ctx, conn, tid, nil, claimer); err != nil {
			return "", err
		}
		ok, err := HeartbeatWorkerNote(k.ctx, conn, tid, k.get("note"), expectedRunID(tid))
		if err != nil {
			return "", err
		}
		if !ok {
			return "", reject(fmt.Sprintf("could not heartbeat %s (unknown id or not running)", tid))
		}
		return okPayload("task_id", tid)
	})
}

// Appends a comment to a task thread under the runtime identity of the caller
func handleComment(k *kanbanCall) (string, error) {
	if err := rejectDelegatedChildMutation(k, "kanban_comment"); err != nil {
		return "", err
	}
	tid := k.get("task_id")
	if !pyrepr.Truthy(tid) {
		return "", reject("task_id is required: comments are per-task and this session has no " +
			"dispatcher-assigned task to default to. Discover task ids with " +
			"kanban_list, or pass the task id you mean.")
	}
	rawBody, err := requireText(k, "body", "")
	if err != nil {
		return "", err
	}
	body := redactText(k.env, rawBody)
	author := persistedIdentity(k.ctx)
	return withBoard(k.ctx, k.get("board"), false, func(conn state.SQLiteConn) (string, error) {
		taskID, err := sqliteBind(tid, 1)
		if err != nil {
			return "", err
		}
		if _, isText := tid.(string); !isText {
			present := false
			if taskID != nil {
				task, err := GetTask(k.ctx, conn, *taskID)
				if err != nil {
					return "", err
				}
				present = task != nil
			}
			if !present {
				return "", &ValueError{"unknown task " + pyrepr.Str(tid)}
			}
		}
		cid, err := AddComment(k.ctx, conn, *taskID, author, body)
		if err != nil {
			return "", err
		}
		return okPayload("task_id", tid, "comment_id", cid)
	})
}
