package kanban

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Keyword arguments of task creation, nil pointers and nil slices standing for None
type CreateTaskOptions struct {
	Title               string
	Body                *string
	Assignee            *string
	CreatedBy           *string
	WorkspaceKind       *string
	WorkspacePath       *string
	BranchName          *string
	Tenant              *string
	Priority            int64
	Parents             []string
	Triage              bool
	IdempotencyKey      *string
	MaxRuntimeSeconds   *int64
	Skills              []string
	MaxRetries          *int64
	ModelOverride       *string
	ProviderOverride    *string
	ReasoningEffort     *string
	GoalMode            bool
	GoalMaxTurns        *int64
	InitialStatus       string
	SessionID           *string
	Board               *string
	ProjectID           *string
	ProjectSourceTaskID *string
	CreatorTaskID       *string
	CompletionContract  *string
}

// Ordered payload object from alternating key and value arguments
func payload(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Python repr of the sorted keys of a set
func sortedRepr(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	items := make([]any, len(keys))
	for i, k := range keys {
		items[i] = k
	}
	return pyrepr.Repr(items)
}

// Equality of two optional strings with None equal only to None
func strPtrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Optional string of a non empty value
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Rows changed by a statement
func rowsAffected(res sql.Result) int64 {
	n, err := res.RowsAffected()
	if err != nil {
		return 0
	}
	return n
}

// Inserted rowid of a statement, zero when the driver reports none
func lastRowID(res sql.Result) int64 {
	n, err := res.LastInsertId()
	if err != nil {
		return 0
	}
	return n
}

// Reports whether an error is the sqlite3 IntegrityError class
func isIntegrityError(err error) bool {
	class, ok := state.SQLiteClassOf(err)
	return ok && class == state.SQLiteIntegrityError
}

// Creates a task under its parents inside one write transaction and returns its id, or the live task holding the idempotency key
func CreateTask(ctx context.Context, conn state.SQLiteConn, o CreateTaskOptions) (string, error) {
	contract, err := validateContract(o.CompletionContract)
	if err != nil {
		return "", err
	}
	modelOverride, providerOverride, err := validateModelOverride(o.ModelOverride, o.ProviderOverride)
	if err != nil {
		return "", err
	}
	reasoningEffort, err := NormalizeReasoningEffort(o.ReasoningEffort)
	if err != nil {
		return "", err
	}
	assignee, err := canonicalAssignee(o.Assignee)
	if err != nil {
		return "", err
	}
	if o.Title == "" || pystr.Strip(o.Title) == "" {
		return "", &ValueError{"title is required"}
	}
	initialStatus := o.InitialStatus
	if initialStatus == "" {
		initialStatus = "running"
	}
	if !ValidInitialStatuses[initialStatus] {
		return "", &ValueError{fmt.Sprintf("initial_status must be one of %s", sortedRepr(ValidInitialStatuses))}
	}
	projectID := o.ProjectID
	if projectID == nil && (o.WorkspaceKind == nil || *o.WorkspaceKind != "scratch") {
		if meta, merr := boardMetaFor(ctx, o.Board); merr == nil {
			if s, ok := objGet(meta, "project_id").(string); ok {
				projectID = nonEmpty(pystr.Strip(s))
			}
		}
	}
	workspaceKind := "scratch"
	if o.WorkspaceKind != nil {
		workspaceKind = *o.WorkspaceKind
	}
	if !ValidWorkspaceKinds[workspaceKind] {
		return "", &ValueError{fmt.Sprintf("workspace_kind must be one of %s, got %s", sortedRepr(ValidWorkspaceKinds), pyrepr.StrRepr(workspaceKind))}
	}
	branchName := o.BranchName
	if branchName != nil {
		branchName = nonEmpty(pystr.Strip(*branchName))
	}
	if branchName != nil && workspaceKind != "worktree" {
		return "", &ValueError{"branch_name is only valid for worktree workspaces"}
	}
	workspacePath := o.WorkspacePath
	link, err := resolveProjectLink(ctx, conn, projectID, o.ProjectSourceTaskID, workspaceKind, workspacePath)
	if err != nil {
		return "", err
	}
	projectID, workspaceKind = link.ProjectID, link.WorkspaceKind
	parents := []string{}
	for _, p := range o.Parents {
		if p != "" {
			parents = append(parents, p)
		}
	}
	skills, err := normalizeTaskSkills(o.Skills)
	if err != nil {
		return "", err
	}
	if o.IdempotencyKey != nil && *o.IdempotencyKey != "" {
		row, err := QueryOne(ctx, conn, "SELECT id FROM tasks WHERE idempotency_key = ? AND status != 'archived' ORDER BY created_at DESC LIMIT 1", *o.IdempotencyKey)
		if err != nil {
			return "", err
		}
		if row != nil {
			return cellStr(row.Get("id")), nil
		}
	}
	now := time.Now().Unix()
	if workspacePath == nil && link.ProjectRepo == nil && (workspaceKind == "dir" || workspaceKind == "worktree") {
		meta, err := boardMetaFor(ctx, o.Board)
		if err != nil {
			return "", err
		}
		if def := objGet(meta, "default_workdir"); truthy(def) {
			s := pyrepr.Str(def)
			workspacePath = &s
		}
	}
	var skillsJSON any
	if skills != nil {
		items := make([]any, len(skills))
		for i, s := range skills {
			items[i] = s
		}
		text, err := pyjson.Dumps(items)
		if err != nil {
			return "", err
		}
		skillsJSON = text
	}
	var skillsPayload any
	if len(skills) > 0 {
		skillsPayload = skills
	}
	var goalPayload any
	if o.GoalMode {
		goalPayload = true
	}
	parentList := make([]any, len(parents))
	for i, p := range parents {
		parentList[i] = p
	}
	for attempt := 0; attempt < 2; attempt++ {
		taskID := newTaskID()
		err := WriteTxn(ctx, conn, true, func() error {
			taskStatus, tenant, err := initialTaskState(ctx, conn, parents, initialStatus, o.Triage, o.Tenant)
			if err != nil {
				return err
			}
			if link.Project != nil && workspaceKind == "worktree" {
				if link.ProjectRepo != nil && *link.ProjectRepo != "" && (workspacePath == nil || *workspacePath == "") {
					p := filepath.Join(*link.ProjectRepo, ".worktrees", taskID)
					workspacePath = &p
				}
				if branchName == nil || *branchName == "" {
					branchName = projectBranchName(link.Project, taskID, o.Title)
				}
			}
			goal := 0
			if o.GoalMode {
				goal = 1
			}
			if _, err := conn.ExecContext(ctx, `
                    INSERT INTO tasks (
                        id, title, body, assignee, status, priority,
                        created_by, created_at, workspace_kind, workspace_path,
                        branch_name, project_id, tenant, idempotency_key,
                        max_runtime_seconds,
                        skills, max_retries, model_override, provider_override,
                        reasoning_effort,
                        goal_mode, goal_max_turns, session_id, completion_contract
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    `, taskID, pystr.Strip(o.Title), optStrValue(o.Body), optStrValue(assignee), taskStatus, o.Priority,
				optStrValue(o.CreatedBy), now, workspaceKind, optStrValue(workspacePath),
				optStrValue(branchName), optStrValue(projectID), optStrValue(tenant), optStrValue(o.IdempotencyKey),
				optIntValue(o.MaxRuntimeSeconds), skillsJSON, optIntValue(o.MaxRetries), optStrValue(modelOverride),
				optStrValue(providerOverride), optStrValue(reasoningEffort), goal, optIntValue(o.GoalMaxTurns),
				optStrValue(o.SessionID), contract); err != nil {
				return err
			}
			for _, pid := range parents {
				if err := linkRow(ctx, conn, pid, taskID); err != nil {
					return err
				}
			}
			if err := appendEvent(ctx, conn, taskID, "created", payload(
				"assignee", assignee, "status", taskStatus, "parents", parentList, "creator_task_id", o.CreatorTaskID,
				"tenant", tenant, "workspace_kind", workspaceKind, "workspace_path", workspacePath,
				"branch_name", branchName, "project_id", projectID, "skills", skillsPayload, "goal_mode", goalPayload,
				"model_override", modelOverride, "provider_override", providerOverride), nil); err != nil {
				return err
			}
			if taskStatus == "blocked" {
				actor := "user"
				if o.CreatedBy != nil && *o.CreatedBy != "" {
					actor = *o.CreatedBy
				}
				if err := appendEvent(ctx, conn, taskID, "blocked", payload("reason", "initial_status", "status", "blocked", "actor", actor), nil); err != nil {
					return err
				}
			}
			if taskStatus == "todo" {
				for _, p := range parents {
					st, err := taskStatusOf(ctx, conn, p)
					if err != nil {
						return err
					}
					if st == nil || (*st != "done" && *st != "archived") {
						if err := appendEvent(ctx, conn, taskID, "dependency_wait", payload("reason", "parent_not_done", "parent", p), nil); err != nil {
							return err
						}
						break
					}
				}
			}
			if err := inheritCreatorOrigin(ctx, conn, taskID, o.CreatorTaskID, now); err != nil {
				return err
			}
			return inheritNotifySubs(ctx, conn, taskID, parents, &now)
		})
		if err == nil {
			return taskID, nil
		}
		if !isIntegrityError(err) || attempt == 1 {
			return "", err
		}
	}
	return "", &RuntimeError{"unreachable"}
}

// Metadata of the named board, or of the current board when the name is empty
func boardMetaFor(ctx context.Context, board *string) (*pyjson.Object, error) {
	if board != nil && *board != "" {
		return ReadBoardMetadata(board)
	}
	cur := GetCurrentBoard(ctx)
	return ReadBoardMetadata(&cur)
}

// Project branch for a worktree task, None when the project naming fails
func projectBranchName(project *tools.Project, taskID, title string) (name *string) {
	defer func() {
		if recover() != nil {
			name = nil
		}
	}()
	b := tools.BranchNameFor(project, taskID, title)
	return &b
}

// Records a parent to child edge, ignoring an existing one
func linkRow(ctx context.Context, conn state.SQLiteConn, parentID, childID string) error {
	_, err := conn.ExecContext(ctx, "INSERT OR IGNORE INTO task_links (parent_id, child_id) VALUES (?, ?)", parentID, childID)
	return err
}

// Ids without a tasks row in their original order
func missingTaskIDs(ctx context.Context, conn state.SQLiteConn, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return []string{}, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := QueryRows(ctx, conn, "SELECT id FROM tasks WHERE id IN ("+sqlMarks(len(ids))+")", args...)
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, r := range rows {
		present[cellStr(r.Get("id"))] = true
	}
	missing := []string{}
	for _, id := range ids {
		if !present[id] {
			missing = append(missing, id)
		}
	}
	return missing, nil
}

// Comma separated question marks for an IN clause
func sqlMarks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// Copies every routing column of the parents notify subscriptions to a child with its cursor caught up
func inheritNotifySubs(ctx context.Context, conn state.SQLiteConn, childID string, parents []string, createdAt *int64) error {
	seen := map[string]bool{}
	var ids []any
	for _, p := range parents {
		if p != "" && !seen[p] {
			seen[p] = true
			ids = append(ids, p)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	row, err := QueryOne(ctx, conn, "SELECT COALESCE(MAX(id), 0) AS cursor FROM task_events WHERE task_id = ?", childID)
	if err != nil {
		return err
	}
	var cursor int64
	if row != nil {
		if cursor, err = pyIntOf(row.Get("cursor")); err != nil {
			return err
		}
	}
	at := time.Now().Unix()
	if createdAt != nil {
		at = *createdAt
	}
	args := append([]any{childID, at, cursor}, ids...)
	_, err = conn.ExecContext(ctx, `
        INSERT OR IGNORE INTO kanban_notify_subs
            (task_id, platform, chat_id, thread_id, user_id, user_id_alt,
             chat_type, notifier_profile, delivery_mode, delivery_metadata,
             created_at, last_event_id)
        SELECT ?, platform, chat_id, thread_id, user_id, user_id_alt,
               COALESCE(chat_type, 'dm'), notifier_profile,
               COALESCE(delivery_mode, 'notify'), delivery_metadata, ?, ?
          FROM kanban_notify_subs
         WHERE task_id IN (`+sqlMarks(len(ids))+`)
        `, args...)
	return err
}

// Task by id, nil when no row matches
func GetTask(ctx context.Context, conn state.SQLiteConn, taskID string) (*Task, error) {
	return getTask(ctx, conn, taskID)
}

// Task by id, nil when no row matches
func getTask(ctx context.Context, conn state.SQLiteConn, taskID string) (*Task, error) {
	row, err := QueryOne(ctx, conn, "SELECT * FROM tasks WHERE id = ?", taskID)
	if err != nil || row == nil {
		return nil, err
	}
	return TaskFromRow(row)
}

// Sort keys accepted by list_tasks mapped to their ORDER BY clauses
var ValidSortOrders = map[string]string{
	"created":        "created_at ASC, id ASC",
	"created-desc":   "created_at DESC, id DESC",
	"priority":       "priority DESC, created_at ASC",
	"priority-desc":  "priority ASC, created_at ASC",
	"status":         "status ASC, created_at ASC",
	"assignee":       "assignee ASC, created_at ASC",
	"title":          "title ASC, id ASC",
	"updated":        "started_at DESC NULLS LAST, created_at DESC",
	"completed-desc": "completed_at DESC NULLS LAST, id DESC",
}

// Filters of a task listing, nil meaning unfiltered and a zero limit meaning unbounded
type ListTasksOptions struct {
	Assignee           *string
	Status             *string
	Tenant             *string
	SessionID          *string
	IncludeArchived    bool
	Limit              int64
	OrderBy            *string
	WorkflowTemplateID *string
	CurrentStepKey     *string
}

// Tasks matching the filters, archived ones only when asked, highest priority first by default
func ListTasks(ctx context.Context, conn state.SQLiteConn, o ListTasksOptions) ([]*Task, error) {
	if o.Status != nil && !ValidStatuses[*o.Status] {
		return nil, &ValueError{fmt.Sprintf("status must be one of %s", sortedRepr(ValidStatuses))}
	}
	assignee, err := canonicalAssignee(o.Assignee)
	if err != nil {
		return nil, err
	}
	query := "SELECT * FROM tasks WHERE 1=1"
	var params []any
	for _, f := range []struct {
		col string
		val *string
	}{{"assignee", assignee}, {"status", o.Status}, {"tenant", o.Tenant}, {"session_id", o.SessionID},
		{"workflow_template_id", o.WorkflowTemplateID}, {"current_step_key", o.CurrentStepKey}} {
		if f.val != nil {
			query += " AND " + f.col + " = ?"
			params = append(params, *f.val)
		}
	}
	if !o.IncludeArchived && (o.Status == nil || *o.Status != "archived") {
		query += " AND status != 'archived'"
	}
	if o.OrderBy != nil {
		key := pystr.Lower(pystr.Strip(*o.OrderBy))
		clause, ok := ValidSortOrders[key]
		if !ok {
			keys := map[string]bool{}
			for k := range ValidSortOrders {
				keys[k] = true
			}
			return nil, &ValueError{fmt.Sprintf("order_by must be one of %s", sortedRepr(keys))}
		}
		query += " ORDER BY " + clause
	} else {
		query += " ORDER BY priority DESC, created_at ASC"
	}
	if o.Limit != 0 {
		query += fmt.Sprintf(" LIMIT %d", o.Limit)
	}
	rows, err := QueryRows(ctx, conn, query, params...)
	if err != nil {
		return nil, err
	}
	return tasksFromRows(rows)
}

// Tasks of each row in order
func tasksFromRows(rows []*Row) ([]*Task, error) {
	out := make([]*Task, 0, len(rows))
	for _, r := range rows {
		t, err := TaskFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// Assigns or reassigns a task, refusing one running under a claim, and fires the updated observer after commit
func AssignTask(ctx context.Context, conn state.SQLiteConn, taskID string, profile *string) (bool, error) {
	profile, err := canonicalAssignee(profile)
	if err != nil {
		return false, err
	}
	found := false
	err = WriteTxn(ctx, conn, false, func() error {
		row, err := QueryOne(ctx, conn, "SELECT status, claim_lock, assignee FROM tasks WHERE id = ?", taskID)
		if err != nil || row == nil {
			return err
		}
		found = true
		if row.Get("claim_lock") != nil && row.Get("status") == "running" {
			return &RuntimeError{fmt.Sprintf("cannot reassign %s: currently running (claimed). Wait for completion or reclaim the stale lock first.", taskID)}
		}
		prev := cellOptStr(row.Get("assignee"))
		if !strPtrEq(prev, profile) {
			_, err = conn.ExecContext(ctx, "UPDATE tasks SET assignee = ?, consecutive_failures = 0, last_failure_error = NULL WHERE id = ?", optStrValue(profile), taskID)
		} else {
			_, err = conn.ExecContext(ctx, "UPDATE tasks SET assignee = ? WHERE id = ?", optStrValue(profile), taskID)
		}
		if err != nil {
			return err
		}
		return appendEvent(ctx, conn, taskID, "assigned", payload("assignee", profile, "from", prev), nil)
	})
	if err != nil || !found {
		return false, err
	}
	NotifyTaskUpdated(ctx, conn, taskID, []string{"assignee"}, "")
	return true, nil
}

// Sets the per task model and provider override, an empty model clearing both
func SetModelOverride(ctx context.Context, conn state.SQLiteConn, taskID string, model, provider *string) (bool, error) {
	model, provider, err := validateModelOverride(model, provider)
	if err != nil {
		return false, err
	}
	return setTaskOverride(ctx, conn, taskID, "UPDATE tasks SET model_override = ?, provider_override = ? WHERE id = ?",
		[]any{optStrValue(model), optStrValue(provider)}, "model_override_set", payload("model", model, "provider", provider),
		[]string{"model_override", "provider_override"}, "cannot set model override")
}

// Writes a per task override refusing archived tasks, records the event and fires the updated observer after commit
func setTaskOverride(ctx context.Context, conn state.SQLiteConn, taskID, query string, params []any, eventKind string, body *pyjson.Object, changed []string, archivedMsg string) (bool, error) {
	found := false
	err := WriteTxn(ctx, conn, false, func() error {
		st, err := taskStatusOf(ctx, conn, taskID)
		if err != nil || st == nil {
			return err
		}
		found = true
		if *st == "archived" {
			return &RuntimeError{fmt.Sprintf("%s on archived task %s", archivedMsg, taskID)}
		}
		if _, err := conn.ExecContext(ctx, query, append(params, taskID)...); err != nil {
			return err
		}
		return appendEvent(ctx, conn, taskID, eventKind, body, nil)
	})
	if err != nil || !found {
		return false, err
	}
	NotifyTaskUpdated(ctx, conn, taskID, changed, "")
	return true, nil
}

// Sets the per task reasoning effort, empty clearing it and none pinning thinking off
func SetReasoningEffort(ctx context.Context, conn state.SQLiteConn, taskID string, effort *string) (bool, error) {
	effort, err := NormalizeReasoningEffort(effort)
	if err != nil {
		return false, err
	}
	return setTaskOverride(ctx, conn, taskID, "UPDATE tasks SET reasoning_effort = ? WHERE id = ?", []any{optStrValue(effort)},
		"reasoning_effort_set", payload("reasoning_effort", effort), []string{"reasoning_effort"}, "cannot set reasoning effort")
}

// Links parent to child, reporting whether the edge gated a ready child back to todo
func LinkTasks(ctx context.Context, conn state.SQLiteConn, parentID, childID string, expectedChildRunID *int64) (bool, error) {
	if parentID == childID {
		return false, &ValueError{"a task cannot depend on itself"}
	}
	gated := false
	err := WriteTxn(ctx, conn, false, func() error {
		missing, err := missingTaskIDs(ctx, conn, []string{parentID, childID})
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return &ValueError{"unknown task(s): " + strings.Join(missing, ", ")}
		}
		child, err := QueryOne(ctx, conn, "SELECT status, current_run_id FROM tasks WHERE id = ?", childID)
		if err != nil {
			return err
		}
		if child.Get("status") == "running" {
			cur := cellOptInt(child.Get("current_run_id"))
			if expectedChildRunID == nil || cur == nil || *cur != *expectedChildRunID {
				return &ValueError{fmt.Sprintf("cannot link %s -> %s: child is already running", parentID, childID)}
			}
		}
		cycle, err := wouldCycle(ctx, conn, parentID, childID)
		if err != nil {
			return err
		}
		if cycle {
			return &ValueError{fmt.Sprintf("linking %s -> %s would create a cycle", parentID, childID)}
		}
		if err := linkRow(ctx, conn, parentID, childID); err != nil {
			return err
		}
		st, err := taskStatusOf(ctx, conn, parentID)
		if err != nil {
			return err
		}
		if st == nil || (*st != "done" && *st != "archived") {
			res, err := conn.ExecContext(ctx, "UPDATE tasks SET status = 'todo' WHERE id = ? AND status = 'ready'", childID)
			if err != nil {
				return err
			}
			gated = rowsAffected(res) == 1
			if gated {
				if err := appendEvent(ctx, conn, childID, "dependency_wait", payload("reason", "parent_not_done", "demoted", true, "parent", parentID), nil); err != nil {
					return err
				}
			}
		}
		if err := appendEvent(ctx, conn, childID, "linked", payload("parent", parentID, "child", childID), nil); err != nil {
			return err
		}
		return inheritNotifySubs(ctx, conn, childID, []string{parentID}, nil)
	})
	return gated, err
}

// Reports whether the parent already descends from the child
func wouldCycle(ctx context.Context, conn state.SQLiteConn, parentID, childID string) (bool, error) {
	seen := map[string]bool{}
	stack := []string{childID}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if node == parentID {
			return true, nil
		}
		if seen[node] {
			continue
		}
		seen[node] = true
		rows, err := QueryRows(ctx, conn, "SELECT child_id FROM task_links WHERE parent_id = ?", node)
		if err != nil {
			return false, err
		}
		for _, r := range rows {
			stack = append(stack, cellStr(r.Get("child_id")))
		}
	}
	return false, nil
}

// Removes a parent to child edge and promotes children it no longer gates
func UnlinkTasks(ctx context.Context, conn state.SQLiteConn, parentID, childID string) (bool, error) {
	removed := false
	err := WriteTxn(ctx, conn, false, func() error {
		res, err := conn.ExecContext(ctx, "DELETE FROM task_links WHERE parent_id = ? AND child_id = ?", parentID, childID)
		if err != nil {
			return err
		}
		removed = rowsAffected(res) > 0
		if removed {
			return appendEvent(ctx, conn, childID, "unlinked", payload("parent", parentID, "child", childID), nil)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if removed {
		if _, err := RecomputeReady(ctx, conn, nil); err != nil {
			return false, err
		}
	}
	return removed, nil
}

// One side of the task links of a task in id order
func linkedIDs(ctx context.Context, conn state.SQLiteConn, want, where, taskID string) ([]string, error) {
	rows, err := QueryRows(ctx, conn, fmt.Sprintf("SELECT %s FROM task_links WHERE %s = ? ORDER BY %s", want, where, want), taskID)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = cellStr(r.Get(want))
	}
	return out, nil
}

// Direct parents of a task in id order
func ParentIDs(ctx context.Context, conn state.SQLiteConn, taskID string) ([]string, error) {
	return linkedIDs(ctx, conn, "parent_id", "child_id", taskID)
}

// Direct children of a task in id order
func ChildIDs(ctx context.Context, conn state.SQLiteConn, taskID string) ([]string, error) {
	return linkedIDs(ctx, conn, "child_id", "parent_id", taskID)
}

// Direct parents and children with title and status for each requested task in request order
func TaskGraphContexts(ctx context.Context, conn state.SQLiteConn, taskIDs []string) (*pyjson.Object, error) {
	contexts := pyjson.NewObject()
	var ordered []any
	for _, id := range taskIDs {
		if id == "" {
			continue
		}
		if _, ok := contexts.Get(id); !ok {
			contexts.Set(id, payload("parents", []any{}, "children", []any{}))
			ordered = append(ordered, id)
		}
	}
	if len(ordered) == 0 {
		return contexts, nil
	}
	for _, side := range [][3]string{{"parents", "child_id", "parent_id"}, {"children", "parent_id", "child_id"}} {
		bucket, own, other := side[0], side[1], side[2]
		rows, err := QueryRows(ctx, conn, fmt.Sprintf("SELECT l.%s AS owner_id, t.id, t.title, t.status FROM task_links l JOIN tasks t ON t.id = l.%s WHERE l.%s IN (%s) ORDER BY l.%s, t.id",
			own, other, own, sqlMarks(len(ordered)), own), ordered...)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			owner, _ := contexts.Get(cellStr(r.Get("owner_id")))
			entry := owner.(*pyjson.Object)
			list, _ := entry.Get(bucket)
			entry.Set(bucket, append(list.([]any), payload("id", r.Get("id"), "title", r.Get("title"), "status", r.Get("status"))))
		}
	}
	return contexts, nil
}

// Direct parents and children with title and status for one task
func TaskGraphContext(ctx context.Context, conn state.SQLiteConn, taskID string) (*pyjson.Object, error) {
	all, err := TaskGraphContexts(ctx, conn, []string{taskID})
	if err != nil {
		return nil, err
	}
	v, _ := all.Get(taskID)
	return v.(*pyjson.Object), nil
}

// Adds a stripped comment with a commented event and returns its id
func AddComment(ctx context.Context, conn state.SQLiteConn, taskID, author, body string) (int64, error) {
	if body == "" || pystr.Strip(body) == "" {
		return 0, &ValueError{"comment body is required"}
	}
	if author == "" || pystr.Strip(author) == "" {
		return 0, &ValueError{"comment author is required"}
	}
	now := time.Now().Unix()
	var id int64
	err := WriteTxn(ctx, conn, true, func() error {
		if err := requireTask(ctx, conn, taskID); err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, "INSERT INTO task_comments (task_id, author, body, created_at) VALUES (?, ?, ?, ?)", taskID, pystr.Strip(author), pystr.Strip(body), now)
		if err != nil {
			return err
		}
		id = lastRowID(res)
		return appendEvent(ctx, conn, taskID, "commented", payload("author", author, "len", utf8.RuneCountInString(body)), nil)
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// Refuses an id without a tasks row
func requireTask(ctx context.Context, conn state.SQLiteConn, taskID string) error {
	row, err := QueryOne(ctx, conn, "SELECT 1 FROM tasks WHERE id = ?", taskID)
	if err != nil {
		return err
	}
	if row == nil {
		return &ValueError{"unknown task " + taskID}
	}
	return nil
}

// Rows of a per task table in the given order
func taskRows(ctx context.Context, conn state.SQLiteConn, table, taskID, order string) ([]*Row, error) {
	return QueryRows(ctx, conn, fmt.Sprintf("SELECT * FROM %s WHERE task_id = ? ORDER BY %s", table, order), taskID)
}

// Comments of a task oldest first
func ListComments(ctx context.Context, conn state.SQLiteConn, taskID string) ([]*Comment, error) {
	rows, err := taskRows(ctx, conn, "task_comments", taskID, "created_at ASC")
	return commentsOf(rows), err
}

// Comments of rows in order
func commentsOf(rows []*Row) []*Comment {
	out := make([]*Comment, len(rows))
	for i, r := range rows {
		out[i] = CommentFromRow(r)
	}
	return out
}

// Comments with a rowid above the cursor in rowid order so a same second burst is never skipped
func ListCommentsAfter(ctx context.Context, conn state.SQLiteConn, taskID string, afterID int64) ([]*Comment, error) {
	rows, err := QueryRows(ctx, conn, "SELECT id, task_id, author, body, created_at FROM task_comments WHERE task_id = ? AND id > ? ORDER BY id ASC", taskID, afterID)
	return commentsOf(rows), err
}

// Attachment over the size cap, unwrapping to ValueError so generic handlers still catch it
type AttachmentTooLarge struct{ Msg string }

func (e *AttachmentTooLarge) Error() string { return e.Msg }
func (e *AttachmentTooLarge) Unwrap() error { return &ValueError{e.Msg} }

// Safe basename of a client filename without directories, control characters or leading dots
func safeAttachmentName(raw string) (string, error) {
	parts := strings.Split(strings.ReplaceAll(raw, "\\", "/"), "/")
	name := pystr.Strip(parts[len(parts)-1])
	var b strings.Builder
	for _, r := range name {
		if pystr.IsPrintable(r) && r != 0 {
			b.WriteRune(r)
		}
	}
	name = pystr.Strip(pystr.LstripChars(pystr.Strip(b.String()), "."))
	if name == "" {
		return "", &ValueError{"invalid attachment filename"}
	}
	if rs := []rune(name); len(rs) > 200 {
		name = string(rs[:200])
	}
	return name, nil
}

// First numbered variant of the name that does not exist in the directory
func collisionFreePath(destDir, safeName string) string {
	stem, ext, found := strings.Cut(safeName, ".")
	dot := ""
	if found {
		dot = "."
	}
	candidate := safeName
	for n := 1; ; n++ {
		if _, err := os.Stat(filepath.Join(destDir, candidate)); err != nil {
			return filepath.Join(destDir, candidate)
		}
		candidate = fmt.Sprintf("%s (%d)%s%s", stem, n, dot, ext)
	}
}

// Options of an attachment write, a nil cap meaning the package default
type StoreAttachmentOptions struct {
	ContentType *string
	UploadedBy  *string
	Board       *string
	MaxBytes    *int64
}

// Writes an attachment blob under the task attachments directory and records its row, removing the blob when the row fails
func StoreAttachmentBytes(ctx context.Context, conn state.SQLiteConn, taskID, filename string, data []byte, o StoreAttachmentOptions) (int64, error) {
	maxBytes := int64(KanbanAttachmentMaxBytes)
	if o.MaxBytes != nil {
		maxBytes = *o.MaxBytes
	}
	if int64(len(data)) > maxBytes {
		mb := maxBytes / (1024 * 1024)
		if maxBytes%(1024*1024) != 0 && maxBytes < 0 {
			mb--
		}
		return 0, &AttachmentTooLarge{fmt.Sprintf("attachment exceeds %d MB limit", mb)}
	}
	safeName, err := safeAttachmentName(filename)
	if err != nil {
		return 0, err
	}
	destDir, err := TaskAttachmentsDir(ctx, taskID, o.Board)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(destDir, 0o777); err != nil {
		return 0, err
	}
	destPath := collisionFreePath(destDir, safeName)
	if err := os.WriteFile(destPath, data, 0o666); err != nil {
		return 0, err
	}
	resolved, err := pypath.Resolve(destPath)
	if err == nil {
		var id int64
		id, err = AddAttachment(ctx, conn, taskID, filepath.Base(destPath), resolved, o.ContentType, int64(len(data)), o.UploadedBy)
		if err == nil {
			return id, nil
		}
	}
	os.Remove(destPath)
	return 0, err
}

// Records the metadata row and attached event of a blob the caller already wrote
func AddAttachment(ctx context.Context, conn state.SQLiteConn, taskID, filename, storedPath string, contentType *string, size int64, uploadedBy *string) (int64, error) {
	if filename == "" || pystr.Strip(filename) == "" {
		return 0, &ValueError{"attachment filename is required"}
	}
	if storedPath == "" || pystr.Strip(storedPath) == "" {
		return 0, &ValueError{"attachment stored_path is required"}
	}
	now := time.Now().Unix()
	var id int64
	err := WriteTxn(ctx, conn, false, func() error {
		if err := requireTask(ctx, conn, taskID); err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, "INSERT INTO task_attachments (task_id, filename, stored_path, content_type, size, uploaded_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			taskID, pystr.Strip(filename), storedPath, optStrValue(contentType), size, optStrValue(uploadedBy), now)
		if err != nil {
			return err
		}
		id = lastRowID(res)
		return appendEvent(ctx, conn, taskID, "attached", payload("filename", pystr.Strip(filename), "size", size, "by", uploadedBy), nil)
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// Attachments of a task oldest first
func ListAttachments(ctx context.Context, conn state.SQLiteConn, taskID string) ([]*Attachment, error) {
	rows, err := taskRows(ctx, conn, "task_attachments", taskID, "created_at ASC, id ASC")
	out := make([]*Attachment, len(rows))
	for i, r := range rows {
		out[i] = AttachmentFromRow(r)
	}
	return out, err
}

// Attachment by id, nil when no row matches
func GetAttachment(ctx context.Context, conn state.SQLiteConn, attachmentID int64) (*Attachment, error) {
	r, err := QueryOne(ctx, conn, "SELECT * FROM task_attachments WHERE id = ?", attachmentID)
	if err != nil || r == nil {
		return nil, err
	}
	return AttachmentFromRow(r), nil
}

// Deletes the attachment row and its blob once no other row references it, nil when no row matched
func DeleteAttachment(ctx context.Context, conn state.SQLiteConn, attachmentID int64) (*Attachment, error) {
	var att *Attachment
	remaining := false
	err := WriteTxn(ctx, conn, false, func() error {
		var err error
		if att, err = GetAttachment(ctx, conn, attachmentID); err != nil || att == nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM task_attachments WHERE id = ?", attachmentID); err != nil {
			return err
		}
		ref, err := QueryOne(ctx, conn, "SELECT 1 FROM task_attachments WHERE stored_path = ? LIMIT 1", att.StoredPath)
		if err != nil {
			return err
		}
		remaining = ref != nil
		return appendEvent(ctx, conn, att.TaskID, "attachment_removed", payload("filename", att.Filename), nil)
	})
	if err != nil || att == nil {
		return nil, err
	}
	if !remaining {
		if fi, err := os.Stat(att.StoredPath); err == nil && fi.Mode().IsRegular() {
			os.Remove(att.StoredPath)
		}
	}
	return att, nil
}

// Events of a task oldest first
func ListEvents(ctx context.Context, conn state.SQLiteConn, taskID string) ([]*Event, error) {
	rows, err := taskRows(ctx, conn, "task_events", taskID, "created_at ASC, id ASC")
	if err != nil {
		return nil, err
	}
	out := make([]*Event, 0, len(rows))
	for _, r := range rows {
		e, err := EventFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// Raw comment insert for callers already inside a write transaction
func insertComment(ctx context.Context, conn state.SQLiteConn, taskID, author, body string, createdAt int64) error {
	_, err := conn.ExecContext(ctx, "INSERT INTO task_comments (task_id, author, body, created_at) VALUES (?, ?, ?, ?)", taskID, author, body, createdAt)
	return err
}

// Inserts an event row inside the caller transaction, a nil run id marking it task scoped
func appendEvent(ctx context.Context, conn state.SQLiteConn, taskID, kind string, body any, runID *int64) error {
	text, err := jsonOrNull(body)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "INSERT INTO task_events (task_id, run_id, kind, payload, created_at) VALUES (?, ?, ?, ?, ?)",
		taskID, optIntValue(runID), kind, text, time.Now().Unix())
	return err
}

// Closes the active run with its outcome and clears current_run_id, nil when no run was active
func endRun(ctx context.Context, conn state.SQLiteConn, taskID, outcome string, summary, errText *string, metadata any, status string) (*int64, error) {
	now := time.Now().Unix()
	runID, err := currentRunID(ctx, conn, taskID)
	if err != nil || runID == nil {
		return nil, err
	}
	if status == "" {
		status = outcome
	}
	meta, err := jsonOrNull(metadata)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `
        UPDATE task_runs
           SET status        = ?,
               outcome       = ?,
               summary       = ?,
               error         = ?,
               metadata      = ?,
               ended_at      = ?,
               claim_expires = NULL
         WHERE id = ?
           AND ended_at IS NULL
        `, status, outcome, optStrValue(summary), optStrValue(errText), meta, now, *runID); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE tasks SET current_run_id = NULL WHERE id = ?", taskID); err != nil {
		return nil, err
	}
	return runID, nil
}

// First line of the stripped text capped at limit characters, empty when there is none
func firstLine(text *string, limit int) string {
	s := ""
	if text != nil {
		s = *text
	}
	lines := pystr.Splitlines(pystr.Strip(s), false)
	if len(lines) == 0 {
		return ""
	}
	if rs := []rune(lines[0]); len(rs) > limit {
		return string(rs[:limit])
	}
	return lines[0]
}

// Integer of a value with None passing through
func optInt(value any) (*int64, error) {
	if value == nil {
		return nil, nil
	}
	n, err := pyIntOf(value)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// JSON text for a payload or metadata column, NULL for an empty or missing value
func jsonOrNull(obj any) (any, error) {
	switch v := obj.(type) {
	case nil:
		return nil, nil
	case *pyjson.Object:
		if v == nil || len(v.Keys) == 0 {
			return nil, nil
		}
	case map[string]any:
		if len(v) == 0 {
			return nil, nil
		}
	}
	text, err := pyjson.Dumps(obj, pyjson.EnsureASCII(false))
	if err != nil {
		return nil, err
	}
	return text, nil
}

// Current status of a task, nil when no row matches
func taskStatusOf(ctx context.Context, conn state.SQLiteConn, taskID string) (*string, error) {
	row, err := QueryOne(ctx, conn, "SELECT status FROM tasks WHERE id = ?", taskID)
	if err != nil || row == nil {
		return nil, err
	}
	return cellOptStr(row.Get("status")), nil
}

// Active run id of a task, nil when none or zero
func currentRunID(ctx context.Context, conn state.SQLiteConn, taskID string) (*int64, error) {
	row, err := QueryOne(ctx, conn, "SELECT current_run_id FROM tasks WHERE id = ?", taskID)
	if err != nil || row == nil || !truthy(row.Get("current_run_id")) {
		return nil, err
	}
	n, err := pyIntOf(row.Get("current_run_id"))
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// Profile override of a synthesized run, an unset value reading the task assignee
type runProfile struct {
	Set   bool
	Value *string
}

// Ends the active run, or synthesizes a zero duration closed run when none was active and asked
func endOrSynthesizeRun(ctx context.Context, conn state.SQLiteConn, taskID, outcome, status string, summary *string, metadata any, synthesize bool, profile runProfile) (*int64, error) {
	runID, err := endRun(ctx, conn, taskID, outcome, summary, nil, metadata, status)
	if err != nil {
		return nil, err
	}
	if runID == nil && synthesize {
		id, err := synthesizeEndedRun(ctx, conn, taskID, outcome, summary, nil, metadata, profile)
		if err != nil {
			return nil, err
		}
		runID = &id
	}
	return runID, nil
}

// Inserts a zero duration closed run for a terminal transition on a never claimed task without touching the task row
func synthesizeEndedRun(ctx context.Context, conn state.SQLiteConn, taskID, outcome string, summary, errText *string, metadata any, profile runProfile) (int64, error) {
	now := time.Now().Unix()
	trow, err := QueryOne(ctx, conn, "SELECT assignee, current_step_key FROM tasks WHERE id = ?", taskID)
	if err != nil {
		return 0, err
	}
	var prof, stepKey any
	if profile.Set {
		prof = optStrValue(profile.Value)
	} else if trow != nil {
		prof = trow.Get("assignee")
	}
	if trow != nil {
		stepKey = trow.Get("current_step_key")
	}
	meta, err := jsonOrNull(metadata)
	if err != nil {
		return 0, err
	}
	res, err := conn.ExecContext(ctx, `
        INSERT INTO task_runs (
            task_id, profile, step_key,
            status, outcome,
            summary, error, metadata,
            started_at, ended_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        `, taskID, prof, stepKey, outcome, outcome, optStrValue(summary), optStrValue(errText), meta, now, now)
	if err != nil {
		return 0, err
	}
	return lastRowID(res), nil
}

// Epoch seconds from a number, a numeric string or an ISO 8601 stamp, nil for empty or invalid values
func toEpoch(val any) *int64 {
	switch v := val.(type) {
	case nil:
		return nil
	case int64:
		return &v
	case int:
		n := int64(v)
		return &n
	case bool:
		n := int64(0)
		if v {
			n = 1
		}
		return &n
	case float64:
		n := int64(v)
		return &n
	}
	s := pystr.Strip(pyrepr.Str(val))
	if s == "" {
		return nil
	}
	if n, err := pyIntOf(s); err == nil {
		return &n
	} else if !errors.As(err, new(*ValueError)) {
		return nil
	}
	t, aware, err := pydatetime.ParseISOFormat(s)
	if err != nil {
		return nil
	}
	if !aware {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
	}
	n := t.Unix()
	return &n
}
