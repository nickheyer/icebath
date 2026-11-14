package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
)

var kanbanCreateTaskModel = &Model{ClassName: "CreateTaskBody", Fields: []ModelField{
	Field("title", Str),
	FieldDefault("body", optStr, nil),
	FieldDefault("assignee", optStr, nil),
	FieldDefault("tenant", optStr, nil),
	FieldDefault("priority", Int, int64(0)),
	FieldDefault("workspace_kind", optStr, nil),
	FieldDefault("workspace_path", optStr, nil),
	FieldFactory("parents", ListOf(Str), func() any { return []any{} }),
	FieldDefault("triage", Bool, false),
	FieldDefault("idempotency_key", optStr, nil),
	FieldDefault("max_runtime_seconds", optInt, nil),
	FieldDefault("skills", Optional(ListOf(Str)), nil),
	FieldDefault("goal_mode", Bool, false),
	FieldDefault("goal_max_turns", optInt, nil),
	FieldDefault("model_override", optStr, nil),
	FieldDefault("provider_override", optStr, nil),
	FieldDefault("reasoning_effort", optStr, nil),
	FieldDefault("project_id", optStr, nil),
}}

var kanbanUpdateTaskModel = &Model{ClassName: "UpdateTaskBody", Fields: []ModelField{
	FieldDefault("status", optStr, nil),
	FieldDefault("assignee", optStr, nil),
	FieldDefault("priority", optInt, nil),
	FieldDefault("title", optStr, nil),
	FieldDefault("body", optStr, nil),
	FieldDefault("result", optStr, nil),
	FieldDefault("block_reason", optStr, nil),
	FieldDefault("summary", optStr, nil),
	FieldDefault("metadata", Optional(DictOf(Any)), nil),
	FieldDefault("model_override", optStr, nil),
	FieldDefault("provider_override", optStr, nil),
	FieldDefault("clear_model_override", Bool, false),
	FieldDefault("reasoning_effort", optStr, nil),
	FieldDefault("clear_reasoning_effort", Bool, false),
}}

var kanbanBulkTaskModel = &Model{ClassName: "BulkTaskBody", Fields: []ModelField{
	Field("ids", ListOf(Str)),
	FieldDefault("status", optStr, nil),
	FieldDefault("assignee", optStr, nil),
	FieldDefault("priority", optInt, nil),
	FieldDefault("archive", Bool, false),
	FieldDefault("result", optStr, nil),
	FieldDefault("summary", optStr, nil),
	FieldDefault("metadata", Optional(DictOf(Any)), nil),
	FieldDefault("reclaim_first", Bool, false),
	FieldDefault("model_override", optStr, nil),
	FieldDefault("provider_override", optStr, nil),
	FieldDefault("clear_model_override", Bool, false),
	FieldDefault("reasoning_effort", optStr, nil),
	FieldDefault("clear_reasoning_effort", Bool, false),
}}

var kanbanCommentModel = &Model{ClassName: "CommentBody", Fields: []ModelField{
	Field("body", Str),
	FieldDefault("author", optStr, "dashboard"),
}}

var kanbanLinkModel = &Model{ClassName: "LinkBody", Fields: []ModelField{Field("parent_id", Str), Field("child_id", Str)}}

const kanbanRunningDirectMsg = "Cannot set status to 'running' directly; use the dispatcher/claim path"

// A status the dashboard may not set through this path, its text shown to the user
type kanbanStatusRejected struct{ msg string }

func (e *kanbanStatusRejected) Error() string { return e.msg }

// Strings of a validated list field
func kanbanStringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, pyrepr.Str(item))
	}
	return out
}

// Metadata dict of a payload, nil when it carries none
func kanbanMetadata(p *pyjson.Object) *pyjson.Object {
	meta, _ := p.GetOr("metadata", nil).(*pyjson.Object)
	return meta
}

// Creates a task from the dashboard and warns when a ready assigned task has no dispatcher to run it
func kanbanCreateTask(ctx context.Context, payload *pyjson.Object, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		createdBy := "dashboard"
		opts := kanban.CreateTaskOptions{
			Title: pyrepr.Str(payload.GetOr("title", "")), Body: kanbanField(payload, "body"), Assignee: kanbanField(payload, "assignee"),
			CreatedBy: &createdBy, WorkspaceKind: kanbanField(payload, "workspace_kind"), WorkspacePath: kanbanField(payload, "workspace_path"),
			Tenant: kanbanField(payload, "tenant"), Priority: kanbanInt(payload.GetOr("priority", int64(0))),
			Parents: kanbanStringList(payload.GetOr("parents", nil)), Triage: payload.GetOr("triage", false) == true,
			IdempotencyKey: kanbanField(payload, "idempotency_key"), MaxRuntimeSeconds: kanbanIntField(payload, "max_runtime_seconds"),
			ModelOverride: kanbanField(payload, "model_override"), ProviderOverride: kanbanField(payload, "provider_override"),
			ReasoningEffort: kanbanField(payload, "reasoning_effort"), GoalMode: payload.GetOr("goal_mode", false) == true,
			GoalMaxTurns: kanbanIntField(payload, "goal_max_turns"), Board: board, ProjectID: kanbanField(payload, "project_id"),
		}
		if skills, ok := payload.GetOr("skills", nil).([]any); ok {
			opts.Skills = kanbanStringList(skills)
		}
		taskID, err := kanban.CreateTask(ctx, conn, opts)
		if err != nil {
			return nil, kanbanValueError400(err)
		}
		task, err := kanban.GetTask(ctx, conn, taskID)
		if err != nil {
			return nil, kanbanValueError400(err)
		}
		body := pyjson.ObjectOf("task", nil)
		if task != nil {
			started, err := kanbanRunStart(ctx, conn, taskID)
			if err != nil {
				return nil, kanbanValueError400(err)
			}
			body.Set("task", kanbanTaskDict(task, nil, started))
		}
		if task != nil && task.Status == "ready" && kanbanText(task.Assignee) != "" {
			if running, message := kanban.CheckDispatcherPresence(ctx, home.Dir(ctx)); !running && message != "" {
				body.Set("warning", message)
			}
		}
		return body, nil
	})
}

// Attachments of a task
func kanbanListTaskAttachments(ctx context.Context, taskID string, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		if _, err := kanbanRequireTask(ctx, conn, taskID); err != nil {
			return nil, err
		}
		attachments, err := kanban.ListAttachments(ctx, conn, taskID)
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(attachments))
		for _, a := range attachments {
			out = append(out, kanbanAttachmentDict(a))
		}
		return pyjson.ObjectOf("attachments", out), nil
	})
}

// Streams an upload into the task attachment dir under the size cap and records it
func kanbanUploadTaskAttachment(ctx context.Context, taskID string, file *UploadFile, board, uploadedBy *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		out, err := func() (any, error) {
			if _, err := kanbanRequireTask(ctx, conn, taskID); err != nil {
				return nil, err
			}
			safeName, err := kanban.SafeAttachmentName(kanbanText(file.Filename))
			if err != nil {
				return nil, err
			}
			destDir, err := kanban.TaskAttachmentsDir(ctx, taskID, board)
			if err != nil {
				return nil, err
			}
			if err := os.MkdirAll(destDir, 0o777); err != nil {
				return nil, err
			}
			destPath := kanban.CollisionFreePath(destDir, safeName)
			total, err := kanbanStoreUpload(file, destPath)
			if err != nil {
				return nil, err
			}
			resolved, err := pypath.Resolve(destPath)
			if err != nil {
				return nil, err
			}
			var contentType *string
			if ct, ok := file.ContentType(); ok {
				contentType = &ct
			}
			by := kanbanText(uploadedBy)
			if by == "" {
				by = "dashboard"
			}
			attID, err := kanban.AddAttachment(ctx, conn, taskID, pypath.PathName(destPath), resolved, contentType, total, &by)
			if err != nil {
				return nil, err
			}
			att, err := kanban.GetAttachment(ctx, conn, attID)
			if err != nil {
				return nil, err
			}
			if att == nil {
				return pyjson.ObjectOf("attachment", nil), nil
			}
			return pyjson.ObjectOf("attachment", kanbanAttachmentDict(att)), nil
		}()
		return out, kanbanValueError400(err)
	})
}

// Copies the upload to its destination in chunks, a 413 past the cap and a 500 for a storage failure
func kanbanStoreUpload(file *UploadFile, destPath string) (int64, error) {
	storeFailed := func(err error) error {
		return NewHTTPException(500, "failed to store attachment: "+pypath.OSErrorText(err))
	}
	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return 0, storeFailed(err)
	}
	var total int64
	for {
		chunk, err := file.Read(1024 * 1024)
		if err != nil {
			out.Close()
			if pypath.IsOSError(err) {
				return 0, storeFailed(err)
			}
			return 0, err
		}
		if len(chunk) == 0 {
			break
		}
		total += int64(len(chunk))
		if total > kanban.KanbanAttachmentMaxBytes {
			out.Close()
			if err := os.Remove(destPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return 0, storeFailed(err)
			}
			return 0, NewHTTPException(413, fmt.Sprintf("attachment exceeds %d MB limit", kanban.KanbanAttachmentMaxBytes/(1024*1024)))
		}
		if _, err := out.Write(chunk); err != nil {
			out.Close()
			return 0, storeFailed(err)
		}
	}
	if err := out.Close(); err != nil {
		return 0, storeFailed(err)
	}
	return total, nil
}

// Serves an attachment blob that still lives under the board attachments root
func kanbanDownloadAttachment(ctx context.Context, attachmentID int64, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		att, err := kanban.GetAttachment(ctx, conn, attachmentID)
		if err != nil {
			return nil, err
		}
		if att == nil {
			return nil, NewHTTPException(404, "attachment not found")
		}
		rootDir, err := kanban.AttachmentsRoot(ctx, board)
		if err != nil {
			return nil, err
		}
		root, err := pypath.Resolve(rootDir)
		if err != nil {
			return nil, err
		}
		stored, err := pypath.Resolve(att.StoredPath)
		if err != nil {
			return nil, NewHTTPException(404, "attachment file unavailable")
		}
		if _, inside := pypath.PathRelativeTo(stored, root); !inside {
			return nil, NewHTTPException(404, "attachment file unavailable")
		}
		if info, err := os.Stat(stored); err != nil || !info.Mode().IsRegular() {
			return nil, NewHTTPException(404, "attachment file missing on disk")
		}
		mediaType := kanbanText(att.ContentType)
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return NewFileResponse(stored, 200, mediaType, &att.Filename, "attachment"), nil
	})
}

// Deletes an attachment record and its blob
func kanbanRemoveAttachment(ctx context.Context, attachmentID int64, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		att, err := kanban.DeleteAttachment(ctx, conn, attachmentID)
		if err != nil {
			return nil, err
		}
		if att == nil {
			return nil, NewHTTPException(404, "attachment not found")
		}
		return pyjson.ObjectOf("ok", true, "id", attachmentID), nil
	})
}

// Direct status write for drag and drop moves with its status event, a leaving run closed as reclaimed
func kanbanSetStatusDirect(ctx context.Context, conn state.SQLiteConn, taskID, newStatus string) (bool, error) {
	var terminations []kanban.WorkerTermination
	effective := newStatus
	written := false
	err := kanban.WriteTxn(ctx, conn, false, func() error {
		prev, err := kanban.QueryOne(ctx, conn,
			"SELECT status, current_run_id, worker_pid, claim_lock, worker_started_at FROM tasks WHERE id = ?", taskID)
		if err != nil || prev == nil {
			return err
		}
		prevStatus := pyrepr.Str(prev.Get("status"))
		var runID *int64
		if pyTruthy(prev.Get("current_run_id")) {
			id := kanbanInt(prev.Get("current_run_id"))
			runID = &id
		}
		if prevStatus == "running" && newStatus == "ready" {
			var current *int64
			if prev.Get("current_run_id") != nil {
				id := kanbanInt(prev.Get("current_run_id"))
				current = &id
			}
			resume, err := kanban.RetryStatusForRun(ctx, conn, taskID, current)
			if err != nil {
				return err
			}
			if resume == "review" {
				satisfied, err := kanban.ParentsSatisfied(ctx, conn, taskID)
				if err != nil {
					return err
				}
				effective = "todo"
				if satisfied {
					effective = "review"
				}
			}
		}
		if effective == "ready" {
			satisfied, err := kanban.ParentsSatisfied(ctx, conn, taskID)
			if err != nil || !satisfied {
				return err
			}
		}
		wasRunning := prevStatus == "running"
		reopening := (prevStatus == "done" || prevStatus == "archived") && effective != "done" && effective != "archived"
		res, err := conn.ExecContext(ctx,
			"UPDATE tasks SET status = ?, "+
				"  claim_lock = CASE WHEN ? = 'running' THEN claim_lock ELSE NULL END, "+
				"  claim_expires = CASE WHEN ? = 'running' THEN claim_expires ELSE NULL END, "+
				"  worker_pid = CASE WHEN ? = 'running' THEN worker_pid ELSE NULL END "+
				"WHERE id = ?", effective, effective, effective, effective, taskID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return err
		}
		var eventRun any
		if wasRunning && effective != "running" && runID != nil {
			summary := fmt.Sprintf("status changed to %s (dashboard/direct)", effective)
			ended, err := kanban.EndRun(ctx, conn, taskID, "reclaimed", &summary, "reclaimed")
			if err != nil {
				return err
			}
			eventRun = kanbanOptAny(ended)
			var lock *string
			if s, ok := prev.Get("claim_lock").(string); ok {
				lock = &s
			}
			terminations = append(terminations, kanban.WorkerTermination{Pid: prev.Get("worker_pid"), ClaimLock: lock, StartedAt: prev.Get("worker_started_at")})
		}
		payload, err := pyjson.Dumps(pyjson.ObjectOf("status", effective, "requested_status", newStatus))
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT INTO task_events (task_id, run_id, kind, payload, created_at) VALUES (?, ?, 'status', ?, ?)",
			taskID, eventRun, payload, time.Now().Unix()); err != nil {
			return err
		}
		if reopening {
			result, err := kanban.InvalidateDescendantsForParentReopen(ctx, conn, taskID, "dashboard")
			if err != nil {
				return err
			}
			terminations = append(terminations, result.Terminations...)
		}
		written = true
		return nil
	})
	if err != nil || !written {
		return false, err
	}
	for _, t := range terminations {
		kanban.TerminateReclaimedWorker(t.Pid, t.ClaimLock, t.StartedAt)
	}
	if effective == "done" || effective == "ready" || effective == "review" {
		if _, err := kanban.RecomputeReady(ctx, conn, nil); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Drag into ready, todo or triage through unblock, review reopen or a direct write as the current state needs
func kanbanDragTo(ctx context.Context, conn state.SQLiteConn, taskID, status string) (bool, error) {
	var current *kanban.Task
	if status != "triage" {
		var err error
		if current, err = kanban.GetTask(ctx, conn, taskID); err != nil {
			return false, err
		}
	}
	if status == "ready" && current != nil && (current.Status == "blocked" || current.Status == "scheduled") {
		return kanban.UnblockTask(ctx, conn, taskID)
	}
	if current != nil && current.Status == "review" {
		return kanban.ReopenReviewTask(ctx, conn, taskID)
	}
	return kanbanSetStatusDirect(ctx, conn, taskID, status)
}

// Runs the verb of a status, rejecting running and any status no verb exists for
func kanbanApplyStatus(ctx context.Context, conn state.SQLiteConn, taskID, status string, p *pyjson.Object, unknownDetail string) (bool, error) {
	switch status {
	case "running":
		return false, &kanbanStatusRejected{kanbanRunningDirectMsg}
	case "done":
		return kanban.CompleteTask(ctx, conn, taskID, kanban.CompleteTaskOptions{Result: kanbanField(p, "result"),
			Summary: kanbanField(p, "summary"), Metadata: kanbanMetadata(p), Force: true})
	case "blocked":
		return kanban.BlockTaskWithKind(ctx, conn, taskID, kanbanField(p, "block_reason"), nil, nil)
	case "scheduled":
		return kanban.ScheduleTask(ctx, conn, taskID, kanbanField(p, "block_reason"), nil)
	case "review":
		ok, _, err := kanban.RequestReview(ctx, conn, taskID, kanban.RequestReviewOptions{Summary: kanbanField(p, "summary"),
			Metadata: kanbanMetadata(p), Reviewer: kanbanNonEmpty(kanbanText(kanbanField(p, "assignee"))), Force: true})
		return ok, err
	case "ready", "todo", "triage":
		return kanbanDragTo(ctx, conn, taskID, status)
	}
	return false, &kanbanStatusRejected{unknownDetail}
}

// Sets the priority of a task through the edit path
func kanbanSetPriority(ctx context.Context, conn state.SQLiteConn, taskID string, priority int64, board *string) error {
	_, err := kanban.EditTask(ctx, conn, taskID, kanban.EditTaskOptions{Priority: &priority, Board: board})
	return err
}

// Applies or clears the model override a payload carries
func kanbanApplyModelOverride(ctx context.Context, conn state.SQLiteConn, taskID string, p *pyjson.Object) (bool, error) {
	var model *string
	if p.GetOr("clear_model_override", false) != true {
		model = kanbanNonEmpty(pystr.Strip(kanbanText(kanbanField(p, "model_override"))))
	}
	return kanban.SetModelOverride(ctx, conn, taskID, model, kanbanField(p, "provider_override"))
}

// Applies or clears the reasoning effort a payload carries
func kanbanApplyReasoningEffort(ctx context.Context, conn state.SQLiteConn, taskID string, p *pyjson.Object) (bool, error) {
	var effort *string
	if p.GetOr("clear_reasoning_effort", false) != true {
		effort = kanbanField(p, "reasoning_effort")
	}
	return kanban.SetReasoningEffort(ctx, conn, taskID, effort)
}

// Override knob a patch and a bulk update share
type kanbanOverrideOp struct {
	wanted  func(p *pyjson.Object) bool
	apply   func(ctx context.Context, conn state.SQLiteConn, taskID string, p *pyjson.Object) (bool, error)
	refused string
}

var kanbanOverrideOps = []kanbanOverrideOp{
	{func(p *pyjson.Object) bool {
		return p.GetOr("clear_model_override", false) == true || p.GetOr("model_override", nil) != nil
	}, kanbanApplyModelOverride, "model override refused"},
	{func(p *pyjson.Object) bool {
		return p.GetOr("clear_reasoning_effort", false) == true || p.GetOr("reasoning_effort", nil) != nil
	}, kanbanApplyReasoningEffort, "reasoning override refused"},
}

// Parent rows not done that block a promotion to ready
func kanbanParentsBlockingReady(ctx context.Context, conn state.SQLiteConn, taskID string) ([]*kanban.Row, error) {
	return kanban.QueryRows(ctx, conn,
		"SELECT t.id, t.title, t.status FROM tasks t "+
			"JOIN task_links l ON l.parent_id = t.id "+
			"WHERE l.child_id = ? AND t.status != 'done'", taskID)
}

// Text naming the open parents behind a refused done or review, empty for another status or no open parent
func kanbanOpenParentRefusal(ctx context.Context, conn state.SQLiteConn, taskID, status string) (string, error) {
	if status != "done" && status != "review" {
		return "", nil
	}
	blockers, err := kanban.UnsatisfiedParents(ctx, conn, taskID)
	if err != nil || len(blockers) == 0 {
		return "", err
	}
	parts := make([]string, 0, len(blockers))
	for _, b := range blockers {
		parts = append(parts, fmt.Sprintf("%s (%s)", b.ID, b.Status))
	}
	return fmt.Sprintf("cannot move %s to %s: unsatisfied parent dependencies: %s; complete the parents first (done or archived)",
		taskID, pyrepr.Repr(status), strings.Join(parts, ", ")), nil
}

// Status phase of a patch, 400 for a rejected verb and 409 naming the blockers of a refused transition
func kanbanPatchStatus(ctx context.Context, conn state.SQLiteConn, taskID string, payload *pyjson.Object, reviewAssigneeDeferred bool) error {
	status := kanbanText(kanbanField(payload, "status"))
	var ok bool
	var err error
	if status == "archived" {
		if ok, err = kanban.ArchiveTask(ctx, conn, taskID, nil); err != nil {
			return err
		}
	} else {
		ok, err = kanbanApplyStatus(ctx, conn, taskID, status, payload, "unknown status: "+status)
		if err != nil {
			var rejected *kanbanStatusRejected
			if errors.As(err, &rejected) {
				return NewHTTPException(400, rejected.msg)
			}
			return kanbanValueError400(err)
		}
		if status == "review" && ok && reviewAssigneeDeferred && kanbanText(kanbanField(payload, "assignee")) == "" {
			if ok, err = kanban.AssignTask(ctx, conn, taskID, nil); err != nil {
				return err
			}
		}
	}
	if ok {
		return nil
	}
	if status == "ready" {
		blockers, err := kanbanParentsBlockingReady(ctx, conn, taskID)
		if err != nil {
			return err
		}
		if len(blockers) > 0 {
			names := make([]string, 0, len(blockers))
			for _, p := range blockers {
				names = append(names, fmt.Sprintf("%s (%s, status=%s)", pyrepr.Repr(p.Get("title")), pyrepr.Str(p.Get("id")), pyrepr.Str(p.Get("status"))))
			}
			return kanbanConflict("Cannot move to 'ready': blocked by parent(s) not done — " + strings.Join(names, ", "))
		}
	}
	refusal, err := kanbanOpenParentRefusal(ctx, conn, taskID, status)
	if err != nil {
		return err
	}
	if refusal == "" {
		refusal = fmt.Sprintf("status transition to %s not valid from current state", pyrepr.Repr(status))
	}
	return kanbanConflict(refusal)
}

// Title and body phase of a patch, one update with its edited event then the field name observer
func kanbanPatchTitleBody(ctx context.Context, conn state.SQLiteConn, taskID string, payload *pyjson.Object, board *string) error {
	title, body := kanbanField(payload, "title"), kanbanField(payload, "body")
	err := kanban.WriteTxn(ctx, conn, false, func() error {
		var sets []string
		var vals []any
		if title != nil {
			if pystr.Strip(*title) == "" {
				return NewHTTPException(400, "title cannot be empty")
			}
			sets = append(sets, "title = ?")
			vals = append(vals, pystr.Strip(*title))
		}
		if body != nil {
			sets = append(sets, "body = ?")
			vals = append(vals, *body)
		}
		vals = append(vals, taskID)
		if _, err := conn.ExecContext(ctx, "UPDATE tasks SET "+strings.Join(sets, ", ")+" WHERE id = ?", vals...); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx,
			"INSERT INTO task_events (task_id, kind, payload, created_at) VALUES (?, 'edited', NULL, ?)", taskID, time.Now().Unix())
		return err
	})
	if err != nil {
		return err
	}
	var changed []string
	if title != nil {
		changed = append(changed, "title")
	}
	if body != nil {
		changed = append(changed, "body")
	}
	kanban.NotifyTaskUpdated(ctx, conn, taskID, changed, kanbanText(board))
	return nil
}

// Applies a task patch phase by phase and answers the updated task
func kanbanUpdateTask(ctx context.Context, taskID string, payload *pyjson.Object, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		if _, err := kanbanRequireTask(ctx, conn, taskID); err != nil {
			return nil, err
		}
		assignee := kanbanField(payload, "assignee")
		status := kanbanField(payload, "status")
		reviewAssigneeDeferred := status != nil && *status == "review" && assignee != nil
		if assignee != nil && !reviewAssigneeDeferred {
			ok, err := kanban.AssignTask(ctx, conn, taskID, kanbanNonEmpty(*assignee))
			if err != nil {
				return nil, kanbanMapErrors(409, err, false, true)
			}
			if err := kanbanRequireOK(ok); err != nil {
				return nil, err
			}
		}
		if status != nil {
			if err := kanbanPatchStatus(ctx, conn, taskID, payload, reviewAssigneeDeferred); err != nil {
				return nil, err
			}
		}
		for _, op := range kanbanOverrideOps {
			if !op.wanted(payload) {
				continue
			}
			ok, err := op.apply(ctx, conn, taskID, payload)
			if err != nil {
				return nil, kanbanMapErrors(400, err, true, true)
			}
			if err := kanbanRequireOK(ok); err != nil {
				return nil, err
			}
		}
		if priority := kanbanIntField(payload, "priority"); priority != nil {
			if err := kanbanSetPriority(ctx, conn, taskID, *priority, board); err != nil {
				return nil, err
			}
		}
		if payload.GetOr("title", nil) != nil || payload.GetOr("body", nil) != nil {
			if err := kanbanPatchTitleBody(ctx, conn, taskID, payload, board); err != nil {
				return nil, err
			}
		}
		updated, err := kanban.GetTask(ctx, conn, taskID)
		if err != nil {
			return nil, err
		}
		if updated == nil {
			return pyjson.ObjectOf("task", nil), nil
		}
		started, err := kanbanRunStart(ctx, conn, taskID)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("task", kanbanTaskDict(updated, nil, started)), nil
	})
}

// Deletes a task
func kanbanDeleteTask(ctx context.Context, taskID string, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		ok, err := kanban.DeleteTask(ctx, conn, taskID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, NewHTTPException(404, fmt.Sprintf("task %s not found", taskID))
		}
		return pyjson.ObjectOf("deleted", true, "task_id", taskID), nil
	})
}

// Adds a dashboard comment to a task
func kanbanAddComment(ctx context.Context, taskID string, payload *pyjson.Object, board *string) (any, error) {
	body := pyrepr.Str(payload.GetOr("body", ""))
	if pystr.Strip(body) == "" {
		return nil, NewHTTPException(400, "body is required")
	}
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		if _, err := kanbanRequireTask(ctx, conn, taskID); err != nil {
			return nil, err
		}
		author := kanbanText(kanbanField(payload, "author"))
		if author == "" {
			author = "dashboard"
		}
		if _, err := kanban.AddComment(ctx, conn, taskID, author, body); err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("ok", true), nil
	})
}

// Links a parent task to a child
func kanbanAddLink(ctx context.Context, payload *pyjson.Object, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		gated, err := kanban.LinkTasks(ctx, conn, pyrepr.Str(payload.GetOr("parent_id", "")), pyrepr.Str(payload.GetOr("child_id", "")), nil)
		if err != nil {
			return nil, kanbanValueError400(err)
		}
		return pyjson.ObjectOf("ok", true, "gated", gated), nil
	})
}

// Removes the link between a parent and a child
func kanbanDeleteLink(ctx context.Context, parentID, childID string, board *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		removed, err := kanban.UnlinkTasks(ctx, conn, parentID, childID)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("ok", removed), nil
	})
}

// Applies the bulk patch to one task, recording refusals in its entry without stopping the remaining steps
func kanbanBulkApplyOne(ctx context.Context, conn state.SQLiteConn, taskID string, payload *pyjson.Object, board *string, entry *pyjson.Object) error {
	refuse := func(reason string) {
		entry.Set("ok", false)
		entry.Set("error", reason)
	}
	archive := payload.GetOr("archive", false) == true
	if archive {
		ok, err := kanban.ArchiveTask(ctx, conn, taskID, nil)
		if err != nil {
			return err
		}
		if !ok {
			refuse("archive refused")
		}
	}
	if status := kanbanField(payload, "status"); status != nil && !archive {
		ok, err := kanbanApplyStatus(ctx, conn, taskID, *status, payload, "unknown status "+pyrepr.Repr(*status))
		if err != nil {
			return err
		}
		if !ok {
			refusal, err := kanbanOpenParentRefusal(ctx, conn, taskID, *status)
			if err != nil {
				return err
			}
			if refusal == "" {
				refusal = fmt.Sprintf("transition to %s refused", pyrepr.Repr(*status))
			}
			refuse(refusal)
		}
	}
	if assignee := kanbanField(payload, "assignee"); assignee != nil {
		var ok bool
		var err error
		if payload.GetOr("reclaim_first", false) == true {
			ok, err = kanban.ReassignTask(ctx, conn, taskID, kanbanNonEmpty(*assignee), true, nil)
		} else {
			ok, err = kanban.AssignTask(ctx, conn, taskID, kanbanNonEmpty(*assignee))
		}
		var runtimeErr *kanban.RuntimeError
		switch {
		case errors.As(err, &runtimeErr):
			refuse(err.Error())
		case err != nil:
			return err
		case !ok:
			refuse("assign refused")
		}
	}
	if priority := kanbanIntField(payload, "priority"); priority != nil {
		if err := kanbanSetPriority(ctx, conn, taskID, *priority, board); err != nil {
			return err
		}
	}
	for _, op := range kanbanOverrideOps {
		if !op.wanted(payload) {
			continue
		}
		ok, err := op.apply(ctx, conn, taskID, payload)
		var valueErr *kanban.ValueError
		var runtimeErr *kanban.RuntimeError
		switch {
		case errors.As(err, &valueErr), errors.As(err, &runtimeErr):
			refuse(err.Error())
		case err != nil:
			return err
		case !ok:
			refuse(op.refused)
		}
	}
	return nil
}

// Applies one patch to every id, each task failing on its own with a per id outcome
func kanbanBulkUpdate(ctx context.Context, payload *pyjson.Object, board *string) (any, error) {
	var ids []string
	for _, id := range kanbanStringList(payload.GetOr("ids", nil)) {
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, NewHTTPException(400, "ids is required")
	}
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		results := make([]any, 0, len(ids))
		for _, taskID := range ids {
			entry := pyjson.ObjectOf("id", taskID, "ok", true)
			task, err := kanban.GetTask(ctx, conn, taskID)
			if err == nil && task == nil {
				entry.Set("ok", false)
				entry.Set("error", "not found")
			} else if err == nil {
				err = kanbanBulkApplyOne(ctx, conn, taskID, payload, board, entry)
			}
			if err != nil {
				entry.Set("ok", false)
				entry.Set("error", pyExcStr(err))
			}
			results = append(results, entry)
		}
		return pyjson.ObjectOf("results", results), nil
	})
}
