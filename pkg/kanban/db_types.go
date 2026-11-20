package kanban

import (
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// One board task row
type Task struct {
	ID                  string
	Title               string
	Body                *string
	Assignee            *string
	Status              string
	Priority            int64
	CreatedBy           *string
	CreatedAt           *int64
	StartedAt           *int64
	CompletedAt         *int64
	WorkspaceKind       string
	WorkspacePath       *string
	ClaimLock           *string
	ClaimExpires        *int64
	Tenant              *string
	BranchName          *string
	ProjectID           *string
	Result              *string
	IdempotencyKey      *string
	ConsecutiveFailures int64
	WorkerPID           *int64
	LastFailureError    *string
	MaxRuntimeSeconds   *int64
	LastHeartbeatAt     *int64
	CurrentRunID        *int64
	WorkflowTemplateID  *string
	CurrentStepKey      *string
	Skills              []string
	SkillsSet           bool
	ModelOverride       *string
	ProviderOverride    *string
	ReasoningEffort     *string
	MaxRetries          *int64
	GoalMode            bool
	GoalMaxTurns        *int64
	SessionID           *string
	BlockKind           *string
	BlockRecurrences    int64
	CompletionContract  *string
}

// Task from a row, epochs parsed from text stamps, skills decoded from JSON and absent columns defaulted
func TaskFromRow(row *Row) (*Task, error) {
	g := func(col string, def any) any { return LossyText(rowGet(row, col, def)) }
	t := &Task{
		ID: cellStr(LossyText(row.Get("id"))), Title: cellStr(LossyText(row.Get("title"))),
		Body: cellOptStr(LossyText(row.Get("body"))), Assignee: cellOptStr(LossyText(row.Get("assignee"))),
		Status: cellStr(LossyText(row.Get("status"))), CreatedBy: cellOptStr(LossyText(row.Get("created_by"))),
		WorkspaceKind: cellStr(LossyText(row.Get("workspace_kind"))), WorkspacePath: cellOptStr(LossyText(row.Get("workspace_path"))),
		ClaimLock: cellOptStr(LossyText(row.Get("claim_lock"))),
		CreatedAt: toEpoch(g("created_at", nil)), StartedAt: toEpoch(g("started_at", nil)),
		CompletedAt: toEpoch(g("completed_at", nil)), ClaimExpires: toEpoch(g("claim_expires", nil)),
		LastHeartbeatAt: toEpoch(g("last_heartbeat_at", nil)),
		BranchName:      cellOptStr(g("branch_name", nil)), ProjectID: cellOptStr(g("project_id", nil)),
		Tenant: cellOptStr(g("tenant", nil)), Result: cellOptStr(g("result", nil)),
		IdempotencyKey: cellOptStr(g("idempotency_key", nil)), WorkerPID: cellOptInt(g("worker_pid", nil)),
		MaxRuntimeSeconds: cellOptInt(g("max_runtime_seconds", nil)), CurrentRunID: cellOptInt(g("current_run_id", nil)),
		WorkflowTemplateID: cellOptStr(g("workflow_template_id", nil)), CurrentStepKey: cellOptStr(g("current_step_key", nil)),
		MaxRetries: cellOptInt(g("max_retries", nil)), SessionID: cellOptStr(g("session_id", nil)),
		CompletionContract: cellOptStr(g("completion_contract", nil)),
		ModelOverride:      cellOptStr(orNil(g("model_override", nil))), ProviderOverride: cellOptStr(orNil(g("provider_override", nil))),
		ReasoningEffort: cellOptStr(orNil(g("reasoning_effort", nil))), GoalMaxTurns: cellOptInt(orNil(g("goal_max_turns", nil))),
		BlockKind:        cellOptStr(orNil(g("block_kind", nil))),
		LastFailureError: cellOptStr(g("last_failure_error", g("last_spawn_error", nil))),
		GoalMode:         truthy(g("goal_mode", nil)),
	}
	t.Priority = cellInt(LossyText(row.Get("priority")))
	t.ConsecutiveFailures = cellInt(g("consecutive_failures", g("spawn_failures", int64(0))))
	if list, ok := jsonOr(g("skills", nil), nil).([]any); ok {
		t.SkillsSet = true
		t.Skills = []string{}
		for _, s := range list {
			if pyrepr.Truthy(s) {
				t.Skills = append(t.Skills, pyrepr.Str(s))
			}
		}
	}
	recurrences := g("block_recurrences", nil)
	if !truthy(recurrences) {
		recurrences = int64(0)
	}
	n, err := pyIntOf(recurrences)
	if err != nil {
		return nil, err
	}
	t.BlockRecurrences = n
	return t, nil
}

// One attempt at a task, opened on claim and closed on complete, block, crash, timeout or reclaim
type Run struct {
	ID                int64
	TaskID            string
	Profile           *string
	StepKey           *string
	Status            string
	ClaimLock         *string
	ClaimExpires      *int64
	WorkerPID         *int64
	MaxRuntimeSeconds *int64
	LastHeartbeatAt   *int64
	StartedAt         int64
	EndedAt           *int64
	Outcome           *string
	Summary           *string
	Metadata          any
	Error             *string
}

// Run from a task_runs row
func RunFromRow(row *Row) (*Run, error) {
	id, err := pyIntOf(row.Get("id"))
	if err != nil {
		return nil, err
	}
	return &Run{
		ID: id, TaskID: cellStr(LossyText(row.Get("task_id"))), Profile: cellOptStr(LossyText(row.Get("profile"))),
		StepKey: cellOptStr(LossyText(row.Get("step_key"))), Status: cellStr(LossyText(row.Get("status"))),
		ClaimLock: cellOptStr(LossyText(row.Get("claim_lock"))), WorkerPID: cellOptInt(LossyText(row.Get("worker_pid"))),
		MaxRuntimeSeconds: cellOptInt(LossyText(row.Get("max_runtime_seconds"))), Outcome: cellOptStr(LossyText(row.Get("outcome"))),
		Summary: cellOptStr(LossyText(row.Get("summary"))), Error: cellOptStr(LossyText(row.Get("error"))),
		ClaimExpires: toEpoch(row.Get("claim_expires")), LastHeartbeatAt: toEpoch(row.Get("last_heartbeat_at")),
		EndedAt: toEpoch(row.Get("ended_at")), StartedAt: epochOrZero(row.Get("started_at")),
		Metadata: jsonOr(LossyText(row.Get("metadata")), nil),
	}, nil
}

// One task comment
type Comment struct {
	ID        int64
	TaskID    string
	Author    string
	Body      string
	CreatedAt int64
}

// Comment from a task_comments row
func CommentFromRow(r *Row) *Comment {
	return &Comment{ID: cellInt(r.Get("id")), TaskID: cellStr(r.Get("task_id")), Author: cellStr(LossyText(r.Get("author"))),
		Body: cellStr(LossyText(r.Get("body"))), CreatedAt: epochOrZero(r.Get("created_at"))}
}

// One stored task attachment
type Attachment struct {
	ID          int64
	TaskID      string
	Filename    string
	StoredPath  string
	ContentType *string
	Size        int64
	UploadedBy  *string
	CreatedAt   int64
}

// Attachment from a task_attachments row
func AttachmentFromRow(r *Row) *Attachment {
	size := r.Get("size")
	if !truthy(size) {
		size = int64(0)
	}
	return &Attachment{ID: cellInt(r.Get("id")), TaskID: cellStr(r.Get("task_id")), Filename: cellStr(r.Get("filename")),
		StoredPath: cellStr(r.Get("stored_path")), ContentType: cellOptStr(r.Get("content_type")), Size: cellInt(size),
		UploadedBy: cellOptStr(r.Get("uploaded_by")), CreatedAt: epochOrZero(r.Get("created_at"))}
}

// One task event
type Event struct {
	ID        int64
	TaskID    string
	Kind      string
	Payload   any
	CreatedAt int64
	RunID     *int64
}

// Event from a task_events row, run_id tolerated absent
func EventFromRow(row *Row) (*Event, error) {
	runID, err := optInt(rowGet(row, "run_id", nil))
	if err != nil {
		return nil, err
	}
	return &Event{ID: cellInt(row.Get("id")), TaskID: cellStr(row.Get("task_id")), Kind: cellStr(LossyText(row.Get("kind"))),
		Payload: jsonOr(LossyText(row.Get("payload")), nil), CreatedAt: epochOrZero(row.Get("created_at")), RunID: runID}, nil
}

// Epoch of a cell or zero when it has none
func epochOrZero(v any) int64 {
	if e := toEpoch(v); e != nil {
		return *e
	}
	return 0
}

// The value, or nil when it is falsy like the or None idiom
func orNil(v any) any {
	if !truthy(v) {
		return nil
	}
	return v
}

// Text of a cell, empty for null
func cellStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Optional text of a cell
func cellOptStr(v any) *string {
	if v == nil {
		return nil
	}
	s := cellStr(v)
	return &s
}

// Integer of a cell, zero for null or an unparseable value
func cellInt(v any) int64 {
	n, _ := pyIntOf(v)
	return n
}

// Optional integer of a cell
func cellOptInt(v any) *int64 {
	if v == nil {
		return nil
	}
	n := cellInt(v)
	return &n
}
