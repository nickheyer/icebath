package kanban

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Status glyph of every default column plus the archived dash
var statusIcons = func() map[string]string {
	icons := map[string]string{}
	for c := range DefaultWorkflow.All() {
		icons[c.Key] = c.Icon
	}
	icons[Archived] = "—"
	return icons
}()

var taskDictFields = []string{
	"id", "title", "body", "assignee", "status", "priority", "tenant",
	"workspace_kind", "workspace_path", "branch_name", "project_id",
	"created_by", "created_at", "started_at", "completed_at", "result",
	"skills", "max_runtime_seconds", "max_retries", "model_override", "provider_override",
	"session_id", "workflow_template_id", "current_step_key", "completion_contract", "last_failure_error",
}

var showRunFields = []string{
	"id", "profile", "step_key", "status", "outcome", "summary", "error",
	"metadata", "worker_pid", "started_at", "ended_at",
}

var runsRunFields = []string{
	"id", "profile", "status", "outcome", "started_at", "ended_at",
	"summary", "error", "metadata", "worker_pid", "step_key",
}

var cliAttachmentFields = []string{"id", "filename", "content_type", "size", "uploaded_by", "stored_path", "created_at"}

// Local minute stamp of an epoch, empty for none or zero
func fmtTS(ts *int64) string {
	if ts == nil || *ts == 0 {
		return ""
	}
	return time.Unix(*ts, 0).Local().Format("2006-01-02 15:04")
}

// Prints a value as two space indented JSON, escaping non ASCII when asked
func printJSON(w io.Writer, obj any, ascii bool) error {
	text, err := pyjson.Dumps(obj, pyjson.Indent(2), pyjson.EnsureASCII(ascii))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, text)
	return err
}

// Prints obj as JSON and reports true when --json was passed
func jsonOut(c *kanbanCLI, obj any, ascii bool) (bool, error) {
	if !nsBool(c.ns, "json") {
		return false, nil
	}
	return true, printJSON(c.w, obj, ascii)
}

// Sorted key=value pairs joined by commas, the empty text when there are none
func fmtCounts(counts map[string]int64, empty string) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	if len(parts) == 0 {
		return empty
	}
	return strings.Join(parts, ", ")
}

// Prints msg to stderr and returns rc
func cliErr(errw io.Writer, msg string, rc int) int {
	fmt.Fprintln(errw, msg)
	return rc
}

// Applies op per id, printing ok lines to stdout and fail lines to stderr, exit 1 when any failed
func bulkApply(c *kanbanCLI, ids []string, op func(string) (bool, error), okMsg, failMsg func(string) string) (int, error) {
	failed := false
	for _, tid := range ids {
		ok, err := op(tid)
		if err != nil {
			return 0, err
		}
		if ok {
			fmt.Fprintln(c.w, okMsg(tid))
		} else {
			failed = true
			fmt.Fprintln(c.errw, failMsg(tid))
		}
	}
	if failed {
		return 1, nil
	}
	return 0, nil
}

// One list line of a task with its glyph, status, assignee, tenant and title
func fmtTaskLine(t *Task) string {
	icon, ok := statusIcons[t.Status]
	if !ok {
		icon = "?"
	}
	assignee := "(unassigned)"
	if t.Assignee != nil && *t.Assignee != "" {
		assignee = *t.Assignee
	}
	tenant := ""
	if t.Tenant != nil && *t.Tenant != "" {
		tenant = " [" + *t.Tenant + "]"
	}
	return fmt.Sprintf("%s %s  %-8s  %-20s%s  %s", icon, t.ID, t.Status, assignee, tenant, t.Title)
}

// Python attribute of a task by field name
func taskAttr(t *Task, name string) any {
	switch name {
	case "id":
		return t.ID
	case "title":
		return t.Title
	case "body":
		return optStrValue(t.Body)
	case "assignee":
		return optStrValue(t.Assignee)
	case "status":
		return t.Status
	case "priority":
		return t.Priority
	case "tenant":
		return optStrValue(t.Tenant)
	case "workspace_kind":
		return t.WorkspaceKind
	case "workspace_path":
		return optStrValue(t.WorkspacePath)
	case "branch_name":
		return optStrValue(t.BranchName)
	case "project_id":
		return optStrValue(t.ProjectID)
	case "created_by":
		return optStrValue(t.CreatedBy)
	case "created_at":
		return optIntValue(t.CreatedAt)
	case "started_at":
		return optIntValue(t.StartedAt)
	case "completed_at":
		return optIntValue(t.CompletedAt)
	case "result":
		return optStrValue(t.Result)
	case "skills":
		if !t.SkillsSet {
			return nil
		}
		return stringsAny(t.Skills)
	case "max_runtime_seconds":
		return optIntValue(t.MaxRuntimeSeconds)
	case "max_retries":
		return optIntValue(t.MaxRetries)
	case "model_override":
		return optStrValue(t.ModelOverride)
	case "provider_override":
		return optStrValue(t.ProviderOverride)
	case "reasoning_effort":
		return optStrValue(t.ReasoningEffort)
	case "session_id":
		return optStrValue(t.SessionID)
	case "workflow_template_id":
		return optStrValue(t.WorkflowTemplateID)
	case "current_step_key":
		return optStrValue(t.CurrentStepKey)
	case "completion_contract":
		return optStrValue(t.CompletionContract)
	case "last_failure_error":
		return optStrValue(t.LastFailureError)
	case "claim_lock":
		return optStrValue(t.ClaimLock)
	case "claim_expires":
		return optIntValue(t.ClaimExpires)
	case "worker_pid":
		return optIntValue(t.WorkerPID)
	case "current_run_id":
		return optIntValue(t.CurrentRunID)
	case "goal_mode":
		return t.GoalMode
	case "goal_max_turns":
		return optIntValue(t.GoalMaxTurns)
	case "block_kind":
		return optStrValue(t.BlockKind)
	}
	panic("kanban: Task has no attribute " + pyrepr.Repr(name))
}

// Python attribute of a run by field name
func runAttr(r *Run, name string) any {
	switch name {
	case "id":
		return r.ID
	case "task_id":
		return r.TaskID
	case "profile":
		return optStrValue(r.Profile)
	case "step_key":
		return optStrValue(r.StepKey)
	case "status":
		return r.Status
	case "outcome":
		return optStrValue(r.Outcome)
	case "summary":
		return optStrValue(r.Summary)
	case "error":
		return optStrValue(r.Error)
	case "metadata":
		return r.Metadata
	case "worker_pid":
		return optIntValue(r.WorkerPID)
	case "started_at":
		return r.StartedAt
	case "ended_at":
		return optIntValue(r.EndedAt)
	}
	panic("kanban: Run has no attribute " + pyrepr.Repr(name))
}

// Python attribute of an attachment by field name
func attachmentAttr(a *Attachment, name string) any {
	switch name {
	case "id":
		return a.ID
	case "filename":
		return a.Filename
	case "content_type":
		return optStrValue(a.ContentType)
	case "size":
		return a.Size
	case "uploaded_by":
		return optStrValue(a.UploadedBy)
	case "stored_path":
		return a.StoredPath
	case "created_at":
		return a.CreatedAt
	}
	panic("kanban: Attachment has no attribute " + pyrepr.Repr(name))
}

// Ordered mapping of the named fields read through attr
func objDict(fields []string, attr func(string) any) *pyjson.Object {
	d := pyjson.NewObject()
	for _, k := range fields {
		d.Set(k, attr(k))
	}
	return d
}

// JSON mapping of a task, skills always a list
func taskToDict(t *Task) *pyjson.Object {
	d := objDict(taskDictFields, func(k string) any { return taskAttr(t, k) })
	d.Set("skills", stringsAny(t.Skills))
	return d
}

// Strings as a JSON list, empty rather than null
func stringsAny(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}
