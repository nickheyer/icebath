package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	kanbanPluginName              = "kanban"
	kanbanCardSummaryPreviewChars = 200
)

var kanbanLog = slog.Default().With("logger", "icebath_dashboard_plugin_kanban")

// Query parameter naming the board, the active board when omitted
var kanbanBoardQuery = QueryParam("board", optStr, nil)

// Optional string parameter of a call, nil when it was not given
func kanbanOpt(c *Call, name string) *string {
	if s, ok := c.Value(name).(string); ok {
		return &s
	}
	return nil
}

// Optional string field of a validated body, nil for null
func kanbanField(body *pyjson.Object, name string) *string {
	if s, ok := body.GetOr(name, nil).(string); ok {
		return &s
	}
	return nil
}

// Optional integer field of a validated body, nil for null
func kanbanIntField(body *pyjson.Object, name string) *int64 {
	if n, ok := body.GetOr(name, nil).(int64); ok {
		return &n
	}
	return nil
}

// Text of an optional string, empty for nil
func kanbanText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// The string itself, nil when it is empty
func kanbanNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// HTTP error with the exception text when the error is one of the named Python classes, else the error itself
func kanbanMapErrors(status int, err error, valueError, runtimeError bool) error {
	if err == nil {
		return nil
	}
	var ve *kanban.ValueError
	if valueError && errors.As(err, &ve) {
		return NewHTTPException(status, err.Error())
	}
	var re *kanban.RuntimeError
	if runtimeError && errors.As(err, &re) {
		return NewHTTPException(status, err.Error())
	}
	return err
}

// Maps a domain validation refusal to a 400 with its text
func kanbanValueError400(err error) error { return kanbanMapErrors(400, err, true, false) }

// Maps any failure but an HTTP error to a 500 carrying the prefix and the failure text
func kanbanErrorsTo500(prefix string, err error) error {
	if err == nil {
		return nil
	}
	var he *HTTPException
	if errors.As(err, &he) {
		return err
	}
	return NewHTTPException(500, prefix+": "+pyExcStr(err))
}

// Conflict answer with a detail
func kanbanConflict(detail string) error { return NewHTTPException(409, detail) }

// Normalised board slug, a 400 for a malformed one
func kanbanNormalizeSlugOr400(slug string) (string, error) {
	normed, err := kanban.NormalizeBoardSlug(&slug)
	return normed, kanbanValueError400(err)
}

// Validated board of a query parameter, nil when omitted, 400 when malformed and 404 when unknown or archived
func kanbanResolveBoard(board *string) (*string, error) {
	if board == nil || *board == "" {
		return nil, nil
	}
	normed, err := kanbanNormalizeSlugOr400(*board)
	if err != nil {
		return nil, err
	}
	if normed == "" {
		return nil, nil
	}
	if normed != kanban.DefaultBoard {
		if !kanban.BoardExists(&normed) {
			return nil, NewHTTPException(404, fmt.Sprintf("board %s does not exist", pyrepr.Repr(normed)))
		}
		meta, err := kanban.ReadBoardMetadata(&normed)
		if err != nil {
			return nil, err
		}
		if pyTruthy(meta.GetOr("archived", nil)) {
			return nil, NewHTTPException(404, fmt.Sprintf("board %s is archived", pyrepr.Repr(normed)))
		}
	}
	return &normed, nil
}

// Normalised path slug of a board that exists, 400 when malformed and 404 when unknown
func kanbanExistingBoardSlug(slug string) (string, error) {
	normed, err := kanbanNormalizeSlugOr400(slug)
	if err != nil {
		return "", err
	}
	if normed == "" || !kanban.BoardExists(&normed) {
		return "", NewHTTPException(404, fmt.Sprintf("board %s does not exist", pyrepr.Repr(slug)))
	}
	return normed, nil
}

// Connection to an already normalised board after an idempotent schema init
func kanbanConn(ctx context.Context, board *string) (state.SQLiteConn, error) {
	if _, err := kanban.InitDB(ctx, "", board); err != nil {
		kanbanLog.Warn(fmt.Sprintf("kanban init_db failed: %s", pyExcStr(err)))
	}
	return kanban.Connect(ctx, "", board)
}

// Runs fn over a connection to the resolved board query parameter and closes it afterwards
func kanbanBoardConn(ctx context.Context, board *string, fn func(board *string, conn state.SQLiteConn) (any, error)) (any, error) {
	resolved, err := kanbanResolveBoard(board)
	if err != nil {
		return nil, err
	}
	conn, err := kanbanConn(ctx, resolved)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return fn(resolved, conn)
}

// Context with the board pinned for helpers that resolve the current board themselves
func kanbanBoardPinned(ctx context.Context, board *string) (context.Context, error) {
	resolved, err := kanbanResolveBoard(board)
	if err != nil {
		return nil, err
	}
	slug := kanban.DefaultBoard
	if resolved != nil && *resolved != "" {
		slug = *resolved
	}
	return kanban.ScopedCurrentBoard(ctx, slug), nil
}

// Task of an id, a 404 naming it when it does not exist
func kanbanRequireTask(ctx context.Context, conn state.SQLiteConn, taskID string) (*kanban.Task, error) {
	task, err := kanban.GetTask(ctx, conn, taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, NewHTTPException(404, fmt.Sprintf("task %s not found", taskID))
	}
	return task, nil
}

// Run of an id, a 404 naming it when it does not exist
func kanbanRequireRun(ctx context.Context, conn state.SQLiteConn, runID int64) (*kanban.Run, error) {
	run, err := kanban.GetRun(ctx, conn, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, NewHTTPException(404, fmt.Sprintf("run %d not found", runID))
	}
	return run, nil
}

// A 404 when a mutator reports the task vanished during the request
func kanbanRequireOK(ok bool) error {
	if !ok {
		return NewHTTPException(404, "task not found")
	}
	return nil
}

// Task dict with its derived age, latest run summary and current run start
func kanbanTaskDict(task *kanban.Task, latestSummary *string, currentRunStartedAt *int64) *pyjson.Object {
	d := kanban.TaskAsDict(task)
	d.Set("age", kanban.TaskAge(task))
	d.Set("latest_summary", kanbanOptAny(latestSummary))
	d.Set("current_run_started_at", kanbanOptAny(currentRunStartedAt))
	return d
}

// Value behind an optional, nil for an absent one
func kanbanOptAny[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// Python int of a stored number, zero for anything that is not one
func kanbanInt(v any) int64 {
	if n, ok := pyrepr.Int(v); ok && n.IsInt64() {
		return n.Int64()
	}
	return 0
}

// Start of the current run of one task, nil when it has none
func kanbanRunStart(ctx context.Context, conn state.SQLiteConn, taskID string) (*int64, error) {
	starts, err := kanban.CurrentRunStartedAts(ctx, conn, []string{taskID})
	if err != nil {
		return nil, err
	}
	if at, ok := starts[taskID]; ok {
		return &at, nil
	}
	return nil, nil
}

// Attachment dict the UI lists and downloads by id
func kanbanAttachmentDict(a *kanban.Attachment) *pyjson.Object {
	return pyjson.ObjectOf("id", a.ID, "task_id", a.TaskID, "filename", a.Filename, "content_type", kanbanOptAny(a.ContentType),
		"size", a.Size, "uploaded_by", kanbanOptAny(a.UploadedBy), "stored_path", a.StoredPath, "created_at", a.CreatedAt)
}

// Card badge summary of a diagnostics list, nil when it is empty
func kanbanWarningsSummary(diagnostics []any) any {
	if len(diagnostics) == 0 {
		return nil
	}
	kinds := pyjson.NewObject()
	var count, latest int64
	highestIdx := -1
	var highestSev any
	for _, raw := range diagnostics {
		d, _ := raw.(*pyjson.Object)
		n := kanbanInt(d.GetOr("count", int64(1)))
		kind := pyrepr.Str(d.GetOr("kind", nil))
		kinds.Set(kind, kanbanInt(kinds.GetOr(kind, int64(0)))+n)
		count += n
		latest = max(latest, kanbanInt(d.GetOr("last_seen_at", nil)))
		if sev, ok := d.GetOr("severity", nil).(string); ok {
			for i, name := range kanban.SeverityOrder {
				if name == sev && i > highestIdx {
					highestIdx, highestSev = i, sev
				}
			}
		}
	}
	return pyjson.ObjectOf("count", count, "kinds", kinds, "latest_at", latest, "highest_severity", highestSev)
}

// Puts the full diagnostics list and its badge summary on a task dict when there are any
func kanbanAttachDiagnostics(taskD *pyjson.Object, diags []any) {
	if len(diags) > 0 {
		taskD.Set("diagnostics", diags)
		taskD.Set("warnings", kanbanWarningsSummary(diags))
	}
}

// Parent and child ids of a task
func kanbanLinksFor(ctx context.Context, conn state.SQLiteConn, taskID string) (parents, children []string, err error) {
	ids := func(col, other string) ([]string, error) {
		rows, err := kanban.QueryRows(ctx, conn, "SELECT "+col+" FROM task_links WHERE "+other+" = ? ORDER BY "+col, taskID)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, pyrepr.Str(r.Get(col)))
		}
		return out, nil
	}
	if parents, err = ids("parent_id", "child_id"); err != nil {
		return nil, nil, err
	}
	children, err = ids("child_id", "parent_id")
	return parents, children, err
}

// Strings as a JSON list
func kanbanStrings(items []string) []any {
	out := make([]any, 0, len(items))
	for _, s := range items {
		out = append(out, s)
	}
	return out
}

// Id, title and status of every linked task that still exists, parents first without repeats
func kanbanLinkTasks(ctx context.Context, conn state.SQLiteConn, parents, children []string) ([]any, error) {
	rows := []any{}
	seen := map[string]bool{}
	for _, taskID := range append(append([]string{}, parents...), children...) {
		if seen[taskID] {
			continue
		}
		seen[taskID] = true
		task, err := kanban.GetTask(ctx, conn, taskID)
		if err != nil {
			return nil, err
		}
		if task != nil {
			rows = append(rows, pyjson.ObjectOf("id", task.ID, "title", task.Title, "status", task.Status))
		}
	}
	return rows, nil
}

// Full board grouped by status column with rollups, diagnostics, tenants and assignees
func kanbanGetBoard(ctx context.Context, tenant *string, includeArchived bool, board, workflowTemplateID, currentStepKey *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		tasks, err := kanban.ListTasks(ctx, conn, kanban.ListTasksOptions{Tenant: tenant, IncludeArchived: includeArchived,
			WorkflowTemplateID: workflowTemplateID, CurrentStepKey: currentStepKey})
		if err != nil {
			return nil, err
		}
		type counts struct{ parents, children int64 }
		linkCounts := map[string]*counts{}
		slot := func(id string) *counts {
			if linkCounts[id] == nil {
				linkCounts[id] = &counts{}
			}
			return linkCounts[id]
		}
		links, err := kanban.QueryRows(ctx, conn, "SELECT parent_id, child_id FROM task_links")
		if err != nil {
			return nil, err
		}
		for _, row := range links {
			slot(pyrepr.Str(row.Get("parent_id"))).children++
			slot(pyrepr.Str(row.Get("child_id"))).parents++
		}
		commentCounts := map[string]any{}
		commentRows, err := kanban.QueryRows(ctx, conn, "SELECT task_id, COUNT(*) AS n FROM task_comments GROUP BY task_id")
		if err != nil {
			return nil, err
		}
		for _, r := range commentRows {
			commentCounts[pyrepr.Str(r.Get("task_id"))] = r.Get("n")
		}
		type tally struct{ done, total int64 }
		progress := map[string]*tally{}
		progressRows, err := kanban.QueryRows(ctx, conn,
			"SELECT l.parent_id AS pid, t.status AS cstatus FROM task_links l JOIN tasks t ON t.id = l.child_id")
		if err != nil {
			return nil, err
		}
		for _, row := range progressRows {
			pid := pyrepr.Str(row.Get("pid"))
			if progress[pid] == nil {
				progress[pid] = &tally{}
			}
			progress[pid].total++
			if row.Get("cstatus") == "done" {
				progress[pid].done++
			}
		}
		_, diagnostics, err := kanban.ComputeTaskDiagnosticDicts(ctx, conn, nil, true)
		if err != nil {
			return nil, err
		}
		latestRow, err := kanban.QueryOne(ctx, conn, "SELECT COALESCE(MAX(id), 0) AS m FROM task_events")
		if err != nil {
			return nil, err
		}
		latestEventID := kanbanInt(latestRow.Get("m"))
		names := kanban.DefaultWorkflow.Keys()
		columns := map[string][]*pyjson.Object{}
		for _, name := range names {
			columns[name] = []*pyjson.Object{}
		}
		if includeArchived {
			if _, known := columns["archived"]; !known {
				names = append(names, "archived")
			}
			columns["archived"] = []*pyjson.Object{}
		}
		ids := make([]string, 0, len(tasks))
		for _, t := range tasks {
			ids = append(ids, t.ID)
		}
		summaries, err := kanban.LatestSummaries(ctx, conn, ids)
		if err != nil {
			return nil, err
		}
		runStarts, err := kanban.CurrentRunStartedAts(ctx, conn, ids)
		if err != nil {
			return nil, err
		}
		for _, t := range tasks {
			var preview *string
			if full := summaries[t.ID]; full != "" {
				if runes := []rune(full); len(runes) > kanbanCardSummaryPreviewChars {
					full = string(runes[:kanbanCardSummaryPreviewChars])
				}
				preview = &full
			}
			var started *int64
			if at, ok := runStarts[t.ID]; ok {
				started = &at
			}
			d := kanbanTaskDict(t, preview, started)
			lc := linkCounts[t.ID]
			if lc == nil {
				lc = &counts{}
			}
			d.Set("link_counts", pyjson.ObjectOf("parents", lc.parents, "children", lc.children))
			if n, ok := commentCounts[t.ID]; ok {
				d.Set("comment_count", n)
			} else {
				d.Set("comment_count", int64(0))
			}
			if p := progress[t.ID]; p != nil {
				d.Set("progress", pyjson.ObjectOf("done", p.done, "total", p.total))
			} else {
				d.Set("progress", nil)
			}
			kanbanAttachDiagnostics(d, diagnostics[t.ID])
			column := t.Status
			if _, known := columns[column]; !known {
				column = "todo"
			}
			columns[column] = append(columns[column], d)
		}
		done := columns["done"]
		sort.SliceStable(done, func(i, j int) bool {
			return pyrepr.Str(done[i].GetOr("id", nil)) > pyrepr.Str(done[j].GetOr("id", nil))
		})
		completedKey := func(d *pyjson.Object) (bool, int64) {
			at, ok := d.GetOr("completed_at", nil).(int64)
			return !ok, -at
		}
		sort.SliceStable(done, func(i, j int) bool {
			ni, ai := completedKey(done[i])
			nj, aj := completedKey(done[j])
			if ni != nj {
				return !ni
			}
			return ai < aj
		})
		tenantRows, err := kanban.QueryRows(ctx, conn, "SELECT DISTINCT tenant FROM tasks WHERE tenant IS NOT NULL ORDER BY tenant")
		if err != nil {
			return nil, err
		}
		tenants := make([]any, 0, len(tenantRows))
		for _, r := range tenantRows {
			tenants = append(tenants, r.Get("tenant"))
		}
		known, err := kanban.KnownAssignees(ctx, conn)
		if err != nil {
			return nil, err
		}
		assignees := make([]any, 0, len(known))
		for _, entry := range known {
			assignees = append(assignees, entry.GetOr("name", nil))
		}
		out := make([]any, 0, len(names))
		for _, name := range names {
			cards := make([]any, 0, len(columns[name]))
			for _, d := range columns[name] {
				cards = append(cards, d)
			}
			out = append(out, pyjson.ObjectOf("name", name, "tasks", cards))
		}
		return pyjson.ObjectOf("columns", out, "tenants", tenants, "assignees", assignees,
			"latest_event_id", latestEventID, "now", time.Now().Unix()), nil
	})
}

// Board read coalesced across a burst of identical polls
var kanbanReadBoard = newCoalescedRead(func(ctx context.Context, args ...any) (any, error) {
	opt := func(v any) *string {
		if s, ok := v.(string); ok {
			return &s
		}
		return nil
	}
	return kanbanGetBoard(ctx, opt(args[0]), args[1].(bool), opt(args[2]), opt(args[3]), opt(args[4]))
})

// One task with its comments, events, attachments, links, child results and runs
func kanbanGetTask(ctx context.Context, taskID string, board, runStateType, runStateName *string) (any, error) {
	return kanbanBoardConn(ctx, board, func(board *string, conn state.SQLiteConn) (any, error) {
		if (runStateType == nil) != (runStateName == nil) {
			return nil, NewHTTPException(400, "run_state_type and run_state_name must be passed together or omitted")
		}
		if runStateType != nil && *runStateType != "status" && *runStateType != "outcome" {
			return nil, NewHTTPException(400, "run_state_type must be 'status' or 'outcome'")
		}
		task, err := kanbanRequireTask(ctx, conn, taskID)
		if err != nil {
			return nil, err
		}
		summary, err := kanban.LatestSummary(ctx, conn, taskID)
		if err != nil {
			return nil, err
		}
		started, err := kanbanRunStart(ctx, conn, taskID)
		if err != nil {
			return nil, err
		}
		taskD := kanbanTaskDict(task, summary, started)
		parents, children, err := kanbanLinksFor(ctx, conn, taskID)
		if err != nil {
			return nil, err
		}
		childSummaries, err := kanban.LatestSummaries(ctx, conn, children)
		if err != nil {
			return nil, err
		}
		childResults := []any{}
		for _, cid := range children {
			child, err := kanban.GetTask(ctx, conn, cid)
			if err != nil {
				return nil, err
			}
			if child == nil {
				continue
			}
			var latest any
			if s, ok := childSummaries[child.ID]; ok {
				latest = s
			}
			childResults = append(childResults, pyjson.ObjectOf("id", child.ID, "title", child.Title, "status", child.Status,
				"latest_summary", latest, "result", kanbanOptAny(child.Result)))
		}
		_, diagnostics, err := kanban.ComputeTaskDiagnosticDicts(ctx, conn, []string{taskID}, false)
		if err != nil {
			return nil, err
		}
		kanbanAttachDiagnostics(taskD, diagnostics[taskID])
		comments, err := kanban.ListComments(ctx, conn, taskID)
		if err != nil {
			return nil, err
		}
		commentDicts := make([]any, 0, len(comments))
		for _, c := range comments {
			commentDicts = append(commentDicts, kanban.CommentAsDict(c))
		}
		events, err := kanban.ListEvents(ctx, conn, taskID)
		if err != nil {
			return nil, err
		}
		eventDicts := make([]any, 0, len(events))
		for _, e := range events {
			eventDicts = append(eventDicts, kanban.EventAsDict(e))
		}
		attachments, err := kanban.ListAttachments(ctx, conn, taskID)
		if err != nil {
			return nil, err
		}
		attachmentDicts := make([]any, 0, len(attachments))
		for _, a := range attachments {
			attachmentDicts = append(attachmentDicts, kanbanAttachmentDict(a))
		}
		linkTasks, err := kanbanLinkTasks(ctx, conn, parents, children)
		if err != nil {
			return nil, err
		}
		runs, err := kanban.ListRuns(ctx, conn, taskID, true, runStateType, runStateName)
		if err != nil {
			return nil, err
		}
		runDicts := make([]any, 0, len(runs))
		for _, r := range runs {
			runDicts = append(runDicts, kanban.RunAsDict(r))
		}
		return pyjson.ObjectOf("task", taskD, "comments", commentDicts, "events", eventDicts, "attachments", attachmentDicts,
			"links", pyjson.ObjectOf("parents", kanbanStrings(parents), "children", kanbanStrings(children)),
			"link_tasks", linkTasks, "child_results", childResults, "runs", runDicts), nil
	})
}
