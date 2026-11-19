package tuigateway

// Params of a method addressed at one live session
type SessionIDParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
}

// Lifecycle state of a delegated child
type SubagentStatus string

func (SubagentStatus) Values() []string {
	return []string{"queued", "running", "completed", "failed", "error", "timeout", "interrupted"}
}

// One live delegated child as the delegate registry holds it
type ActiveSubagent struct {
	SubagentID          string          `json:"subagent_id"`
	ParentID            *string         `json:"parent_id,omitzero"`
	Depth               *int64          `json:"depth,omitzero"`
	Goal                *string         `json:"goal,omitzero"`
	DelegationID        *string         `json:"delegation_id,omitzero"`
	Model               *string         `json:"model,omitzero"`
	StartedAt           *float64        `json:"started_at,omitzero"`
	Status              *SubagentStatus `json:"status,omitzero"`
	ToolCount           *int64          `json:"tool_count,omitzero"`
	LastTool            *string         `json:"last_tool,omitzero"`
	OwnerAgentSessionID *string         `json:"owner_agent_session_id,omitzero"`
}

// Live children with the spawn pause and the depth and concurrency limits
type DelegationStatusResult struct {
	Active                []ActiveSubagent `json:"active"`
	Paused                bool             `json:"paused"`
	MaxSpawnDepth         int64            `json:"max_spawn_depth"`
	MaxConcurrentChildren int64            `json:"max_concurrent_children"`
}

// Spawn pause to set, true when absent, the session the canonical route authorizes against
type DelegationPauseParams struct {
	Profile   *string `json:"profile,omitzero"`
	SessionID *string `json:"session_id,omitzero"`
	Paused    *bool   `json:"paused,omitzero"`
}

type DelegationPauseResult struct {
	Paused bool `json:"paused"`
}

type SubagentSteerParams struct {
	SessionID  string  `json:"session_id"`
	Profile    *string `json:"profile,omitzero"`
	SubagentID string  `json:"subagent_id"`
	Text       string  `json:"text"`
}

// Whether a steer reached the child's queue, queued is not delivered
type SteerStatus string

func (SteerStatus) Values() []string { return []string{"queued", "rejected"} }

type SubagentSteerResult struct {
	Status     SteerStatus `json:"status"`
	SubagentID string      `json:"subagent_id"`
	Text       string      `json:"text"`
}

// One owned live child as the subagent list projects it
type SubagentSnapshot struct {
	SubagentID     string          `json:"subagent_id"`
	ParentID       *string         `json:"parent_id,omitzero"`
	Depth          *int64          `json:"depth,omitzero"`
	Goal           *string         `json:"goal,omitzero"`
	DelegationID   *string         `json:"delegation_id,omitzero"`
	Model          *string         `json:"model,omitzero"`
	StartedAt      *float64        `json:"started_at,omitzero"`
	Status         *SubagentStatus `json:"status,omitzero"`
	ToolCount      *int64          `json:"tool_count,omitzero"`
	LastTool       *string         `json:"last_tool,omitzero"`
	AcceptingSteer *bool           `json:"accepting_steer,omitzero"`
}

// One failed task of an async delegation from the durable store
type FailedDelegation struct {
	DelegationID string   `json:"delegation_id"`
	TaskIndex    int64    `json:"task_index,omitzero"`
	Status       string   `json:"status"`
	Goal         string   `json:"goal,omitzero"`
	Error        *string  `json:"error,omitzero"`
	DispatchedAt *float64 `json:"dispatched_at,omitzero"`
	CompletedAt  *float64 `json:"completed_at,omitzero"`
}

// Owned live children and the session's recently failed delegations, newest first
type SubagentListResult struct {
	Subagents   []SubagentSnapshot `json:"subagents"`
	Delegations []FailedDelegation `json:"delegations"`
}

type SubagentIDParams struct {
	SessionID  string  `json:"session_id"`
	Profile    *string `json:"profile,omitzero"`
	SubagentID string  `json:"subagent_id"`
}

// Whether an owned child was still running to interrupt
type SubagentInterruptResult struct {
	Found      bool   `json:"found"`
	SubagentID string `json:"subagent_id"`
}

// Last bytes of a child's transcript, available false when the child recorded none
type SubagentTailResult struct {
	SubagentID string `json:"subagent_id"`
	Available  bool   `json:"available"`
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
}

// One tool result of a finished child's output tail
type SubagentOutputTailEntry struct {
	Tool    string `json:"tool"`
	Preview string `json:"preview"`
	IsError bool   `json:"is_error"`
}

// Every subagent frame, the identity fields absent from emitters that do not know them
type SubagentEventPayload struct {
	Goal            string                    `json:"goal"`
	TaskCount       int64                     `json:"task_count"`
	TaskIndex       int64                     `json:"task_index"`
	SubagentID      *string                   `json:"subagent_id,omitzero"`
	ParentID        *string                   `json:"parent_id,omitzero"`
	ChildSessionID  *string                   `json:"child_session_id,omitzero"`
	DelegationID    *string                   `json:"delegation_id,omitzero"`
	Depth           *int64                    `json:"depth,omitzero"`
	Model           *string                   `json:"model,omitzero"`
	ToolCount       *int64                    `json:"tool_count,omitzero"`
	Toolsets        []string                  `json:"toolsets,omitzero"`
	InputTokens     *int64                    `json:"input_tokens,omitzero"`
	OutputTokens    *int64                    `json:"output_tokens,omitzero"`
	ReasoningTokens *int64                    `json:"reasoning_tokens,omitzero"`
	APICalls        *int64                    `json:"api_calls,omitzero"`
	CostUSD         *float64                  `json:"cost_usd,omitzero"`
	FilesRead       []string                  `json:"files_read,omitzero"`
	FilesWritten    []string                  `json:"files_written,omitzero"`
	OutputTail      []SubagentOutputTailEntry `json:"output_tail,omitzero"`
	ToolName        *string                   `json:"tool_name,omitzero"`
	Text            *string                   `json:"text,omitzero"`
	Status          *SubagentStatus           `json:"status,omitzero"`
	Summary         *string                   `json:"summary,omitzero"`
	DurationSeconds *float64                  `json:"duration_seconds,omitzero"`
	ToolPreview     *string                   `json:"tool_preview,omitzero"`
}

// One tool result of a saved subagent's output tail
type SpawnTreeOutput struct {
	IsError bool   `json:"isError"`
	Preview string `json:"preview"`
	Tool    string `json:"tool"`
}

// One subagent of a saved spawn tree as the terminal UI writes it, its numbers as earlier builds stored them
type SpawnTreeSubagent struct {
	ID              string            `json:"id"`
	Goal            string            `json:"goal"`
	Status          SubagentStatus    `json:"status"`
	Depth           float64           `json:"depth"`
	Index           float64           `json:"index"`
	TaskCount       float64           `json:"taskCount"`
	ToolCount       float64           `json:"toolCount"`
	ParentID        *string           `json:"parentId,omitzero"`
	Notes           []string          `json:"notes"`
	Thinking        []string          `json:"thinking"`
	Tools           []string          `json:"tools"`
	APICalls        *float64          `json:"apiCalls,omitzero"`
	CostUSD         *float64          `json:"costUsd,omitzero"`
	DurationSeconds *float64          `json:"durationSeconds,omitzero"`
	InputTokens     *float64          `json:"inputTokens,omitzero"`
	Iteration       *float64          `json:"iteration,omitzero"`
	OutputTokens    *float64          `json:"outputTokens,omitzero"`
	ReasoningTokens *float64          `json:"reasoningTokens,omitzero"`
	StartedAt       *float64          `json:"startedAt,omitzero"`
	DelegationID    string            `json:"delegationId,omitzero"`
	Model           string            `json:"model,omitzero"`
	Summary         string            `json:"summary,omitzero"`
	FilesRead       []string          `json:"filesRead,omitzero"`
	FilesWritten    []string          `json:"filesWritten,omitzero"`
	Toolsets        []string          `json:"toolsets,omitzero"`
	OutputTail      []SpawnTreeOutput `json:"outputTail,omitzero"`
}

// Session whose processes a stop kills, every process of a standalone sidecar when absent
type ProcessStopParams struct {
	SessionID *string `json:"session_id,omitzero"`
	Profile   *string `json:"profile,omitzero"`
}

// Processes killed and background delegations interrupted
type ProcessStopResult struct {
	Killed      int64 `json:"killed"`
	Delegations int64 `json:"delegations,omitzero"`
}

// One background process the session owns, with the tail of its output
type ProcessEntry struct {
	SessionID        string         `json:"session_id"`
	Command          string         `json:"command"`
	Status           string         `json:"status"`
	Cwd              *string        `json:"cwd,omitzero"`
	PID              *int64         `json:"pid,omitzero"`
	OwnerTaskID      *string        `json:"owner_task_id,omitzero"`
	StartedAt        *string        `json:"started_at,omitzero"`
	StartedEpoch     *float64       `json:"started_epoch,omitzero"`
	UptimeSeconds    *int64         `json:"uptime_seconds,omitzero"`
	OutputPreview    *string        `json:"output_preview,omitzero"`
	OutputTail       *string        `json:"output_tail,omitzero"`
	SessionScoped    *bool          `json:"session_scoped,omitzero"`
	WatchPatterns    []string       `json:"watch_patterns,omitzero"`
	WatchHit         *bool          `json:"watch_hit,omitzero"`
	NotifyOnComplete *bool          `json:"notify_on_complete,omitzero"`
	PersistOnRelease *bool          `json:"persist_on_release,omitzero"`
	WSLChain         *bool          `json:"wsl_chain,omitzero"`
	WSLNote          *string        `json:"wsl_note,omitzero"`
	ExitCode         *int64         `json:"exit_code,omitzero"`
	ExitedAt         *float64       `json:"exited_at,omitzero"`
	CompletionReason *string        `json:"completion_reason,omitzero"`
	Detached         *bool          `json:"detached,omitzero"`
	Extra            map[string]any `json:"-" wire:"extra"`
}

type ProcessListResult struct {
	Processes []ProcessEntry `json:"processes"`
}

type ProcessKillParams struct {
	SessionID string  `json:"session_id"`
	ProcessID string  `json:"process_id"`
	Profile   *string `json:"profile,omitzero"`
}

// Outcome of a kill, error riding on the failure statuses
type ProcessKillStatus string

func (ProcessKillStatus) Values() []string {
	return []string{"killed", "already_exited", "not_found", "error"}
}

// Kill outcome with the process's output snapshot, the registry's further facts kept open
type ProcessKillResult struct {
	Status            ProcessKillStatus `json:"status"`
	SessionID         *string           `json:"session_id,omitzero"`
	Command           *string           `json:"command,omitzero"`
	ExitCode          *int64            `json:"exit_code,omitzero"`
	CompletionReason  *string           `json:"completion_reason,omitzero"`
	TerminationSource *string           `json:"termination_source,omitzero"`
	Output            *string           `json:"output,omitzero"`
	Error             *string           `json:"error,omitzero"`
	Extra             map[string]any    `json:"-" wire:"extra"`
}

// Live delegated children, the spawn pause flag and the limits
var DelegationStatus = NewMethod[ProfileParams, DelegationStatusResult]("delegation.status")

// Blocks or unblocks new spawns everywhere, running children go on, answering the new state
var DelegationPause = NewMethod[DelegationPauseParams, DelegationPauseResult]("delegation.pause")

// Queues steering text into a live delegated child this session owns
var SubagentSteer = NewMethod[SubagentSteerParams, SubagentSteerResult]("subagent.steer")

// Live children this session owns and its failed delegations
var SubagentList = NewMethod[SessionIDParams, SubagentListResult]("subagent.list")

// Hard interrupts one owned child, found false when it already finished
var SubagentInterrupt = NewMethod[SubagentIDParams, SubagentInterruptResult]("subagent.interrupt")

// Last 16 KB of the transcript of a child the conversation spawned, running or finished
var SubagentTail = NewMethod[SubagentIDParams, SubagentTailResult]("subagent.tail")

// Kills the background processes the session owns, or every process a standalone sidecar holds
var ProcessStop = NewMethod[ProcessStopParams, ProcessStopResult]("process.stop")

// Background processes the session owns
var ProcessList = NewMethod[SessionIDParams, ProcessListResult]("process.list")

// Kills one background process the session owns and answers its output snapshot
var ProcessKill = NewMethod[ProcessKillParams, ProcessKillResult]("process.kill")

// A delegate accepted a child goal before the child starts
var SubagentSpawnRequestedEvent = NewEvent[SubagentEventPayload]("subagent.spawn_requested")

// A delegated child started running
var SubagentStartEvent = NewEvent[SubagentEventPayload]("subagent.start")

// Batched tool names a child ran
var SubagentProgressEvent = NewEvent[SubagentEventPayload]("subagent.progress")

// A reasoning chunk of a child
var SubagentThinkingEvent = NewEvent[SubagentEventPayload]("subagent.thinking")

// A child called a tool
var SubagentToolEvent = NewEvent[SubagentEventPayload]("subagent.tool")

// A child finished with its status and usage rollup
var SubagentCompleteEvent = NewEvent[SubagentEventPayload]("subagent.complete")
