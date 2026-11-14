package kanban

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Value behind an optional field, nil for an absent one
func optValue[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// Task fields in dataclass order, the dict a dashboard payload starts from
func TaskAsDict(t *Task) *pyjson.Object {
	var skills any
	if t.SkillsSet {
		skills = stringsAny(t.Skills)
	}
	return pyjson.ObjectOf("id", t.ID, "title", t.Title, "body", optValue(t.Body), "assignee", optValue(t.Assignee),
		"status", t.Status, "priority", t.Priority, "created_by", optValue(t.CreatedBy), "created_at", optValue(t.CreatedAt),
		"started_at", optValue(t.StartedAt), "completed_at", optValue(t.CompletedAt), "workspace_kind", t.WorkspaceKind,
		"workspace_path", optValue(t.WorkspacePath), "claim_lock", optValue(t.ClaimLock), "claim_expires", optValue(t.ClaimExpires),
		"tenant", optValue(t.Tenant), "branch_name", optValue(t.BranchName), "project_id", optValue(t.ProjectID),
		"result", optValue(t.Result), "idempotency_key", optValue(t.IdempotencyKey), "consecutive_failures", t.ConsecutiveFailures,
		"worker_pid", optValue(t.WorkerPID), "last_failure_error", optValue(t.LastFailureError),
		"max_runtime_seconds", optValue(t.MaxRuntimeSeconds), "last_heartbeat_at", optValue(t.LastHeartbeatAt),
		"current_run_id", optValue(t.CurrentRunID), "workflow_template_id", optValue(t.WorkflowTemplateID),
		"current_step_key", optValue(t.CurrentStepKey), "skills", skills, "model_override", optValue(t.ModelOverride),
		"provider_override", optValue(t.ProviderOverride), "reasoning_effort", optValue(t.ReasoningEffort),
		"max_retries", optValue(t.MaxRetries), "goal_mode", t.GoalMode, "goal_max_turns", optValue(t.GoalMaxTurns),
		"session_id", optValue(t.SessionID), "block_kind", optValue(t.BlockKind), "block_recurrences", t.BlockRecurrences,
		"completion_contract", optValue(t.CompletionContract))
}

// Run fields in dataclass order
func RunAsDict(r *Run) *pyjson.Object {
	return pyjson.ObjectOf("id", r.ID, "task_id", r.TaskID, "profile", optValue(r.Profile), "step_key", optValue(r.StepKey),
		"status", r.Status, "claim_lock", optValue(r.ClaimLock), "claim_expires", optValue(r.ClaimExpires),
		"worker_pid", optValue(r.WorkerPID), "max_runtime_seconds", optValue(r.MaxRuntimeSeconds),
		"last_heartbeat_at", optValue(r.LastHeartbeatAt), "started_at", r.StartedAt, "ended_at", optValue(r.EndedAt),
		"outcome", optValue(r.Outcome), "summary", optValue(r.Summary), "metadata", r.Metadata, "error", optValue(r.Error))
}

// Comment fields in dataclass order
func CommentAsDict(c *Comment) *pyjson.Object {
	return pyjson.ObjectOf("id", c.ID, "task_id", c.TaskID, "author", c.Author, "body", c.Body, "created_at", c.CreatedAt)
}

// Event fields in dataclass order
func EventAsDict(e *Event) *pyjson.Object {
	return pyjson.ObjectOf("id", e.ID, "task_id", e.TaskID, "kind", e.Kind, "payload", e.Payload, "created_at", e.CreatedAt,
		"run_id", optValue(e.RunID))
}

// Dispatch pass outcome in dataclass order with tuples as lists
func DispatchResultAsDict(r *DispatchResult) *pyjson.Object {
	spawned := make([]any, 0, len(r.Spawned))
	for _, s := range r.Spawned {
		spawned = append(spawned, []any{s.TaskID, s.Assignee, s.WorkspacePath})
	}
	capped := make([]any, 0, len(r.SkippedPerProfileCapped))
	for _, c := range r.SkippedPerProfileCapped {
		capped = append(capped, []any{c.TaskID, c.Assignee, int64(c.Running)})
	}
	guarded := make([]any, 0, len(r.RespawnGuarded))
	for _, g := range r.RespawnGuarded {
		guarded = append(guarded, []any{g.TaskID, g.Reason})
	}
	return pyjson.ObjectOf("reclaimed", int64(r.Reclaimed), "promoted", int64(r.Promoted),
		"reconciled_orphans", stringsAny(r.ReconciledOrphans), "reaped_terminal_workers", stringsAny(r.ReapedTerminalWorkers),
		"spawned", spawned, "skipped_unassigned", stringsAny(r.SkippedUnassigned),
		"auto_assigned_default", stringsAny(r.AutoAssignedDefault), "skipped_nonspawnable", stringsAny(r.SkippedNonspawnable),
		"skipped_per_profile_capped", capped, "crashed", stringsAny(r.Crashed), "auto_blocked", stringsAny(r.AutoBlocked),
		"timed_out", stringsAny(r.TimedOut), "stale", stringsAny(r.Stale), "respawn_guarded", guarded,
		"rate_limited", stringsAny(r.RateLimited), "skipped_locked", r.SkippedLocked, "memory_pressure", optValue(r.MemoryPressure))
}

// Sanitised attachment file name, a ValueError for a name that cannot be stored
func SafeAttachmentName(raw string) (string, error) { return safeAttachmentName(raw) }

// Path under the directory that no existing attachment holds
func CollisionFreePath(destDir, safeName string) string { return collisionFreePath(destDir, safeName) }

// True when every parent of the task is done or archived
func ParentsSatisfied(ctx context.Context, conn state.SQLiteConn, taskID string) (bool, error) {
	return parentsSatisfied(ctx, conn, taskID)
}

// Closes the open run of a task with an outcome and returns its id
func EndRun(ctx context.Context, conn state.SQLiteConn, taskID, outcome string, summary *string, status string) (*int64, error) {
	return endRun(ctx, conn, taskID, outcome, summary, nil, nil, status)
}

// Stops the worker a reclaimed claim named once its transaction committed
func TerminateReclaimedWorker(pid any, claimLock *string, startedAt any) {
	terminateReclaimedWorkerFn(pid, claimLock, nil, startedAt)
}

// Git work tree root holding the path, nil outside a repository
func GitToplevel(path string) *string { return gitToplevel(path) }

// Loaded runtime config of the active home
func LoadConfigTree(ctx context.Context) (config.Tree, error) {
	return loadKanbanConfigTree(ctx, false)
}

// Active diagnostics per task as dicts in task row order, every live task when all is set
func ComputeTaskDiagnosticDicts(ctx context.Context, conn state.SQLiteConn, taskIDs []string, all bool) ([]string, map[string][]any, error) {
	out := map[string][]any{}
	if !all && len(taskIDs) == 0 {
		return nil, out, nil
	}
	cfg, err := loadKanbanConfigTree(ctx, false)
	if err != nil {
		return nil, nil, err
	}
	diagConfig, err := ConfigFromRuntimeConfig(map[string]any(cfg))
	if err != nil {
		return nil, nil, err
	}
	var rows []*Row
	if all {
		rows, err = QueryRows(ctx, conn, "SELECT * FROM tasks WHERE status != 'archived'")
	} else {
		args := make([]any, 0, len(taskIDs))
		for _, id := range taskIDs {
			args = append(args, id)
		}
		rows, err = QueryRows(ctx, conn, "SELECT * FROM tasks WHERE id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(taskIDs)), ",")+")", args...)
	}
	if err != nil || len(rows) == 0 {
		return nil, out, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, pyrepr.Str(r.Get("id")))
	}
	events, err := rowsByTask(ctx, conn, "task_events", ids)
	if err != nil {
		return nil, nil, err
	}
	runs, err := rowsByTask(ctx, conn, "task_runs", ids)
	if err != nil {
		return nil, nil, err
	}
	graphs, err := TaskGraphContexts(ctx, conn, ids)
	if err != nil {
		return nil, nil, err
	}
	var order []string
	for _, r := range rows {
		tid := pyrepr.Str(r.Get("id"))
		var graph any
		if g, ok := graphs.Get(tid); ok && g != nil {
			graph = g
		}
		diags := ComputeTaskDiagnostics(r, events[tid], runs[tid], nil, diagConfig, graph)
		if len(diags) == 0 {
			continue
		}
		dicts := make([]any, 0, len(diags))
		for _, d := range diags {
			dicts = append(dicts, d.ToDict())
		}
		if _, seen := out[tid]; !seen {
			order = append(order, tid)
		}
		out[tid] = dicts
	}
	return order, out, nil
}
