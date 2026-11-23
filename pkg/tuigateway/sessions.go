package tuigateway

type TerminalResizeResult struct {
	Cols int64 `json:"cols"`
}

type TerminalResizeParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
	Cols      *int64  `json:"cols,omitzero"`
}

type SpawnTreeLoadResult struct {
	SessionID  *string             `json:"session_id,omitzero"`
	StartedAt  *float64            `json:"started_at,omitzero"`
	FinishedAt *float64            `json:"finished_at,omitzero"`
	Label      *string             `json:"label,omitzero"`
	Subagents  []SpawnTreeSubagent `json:"subagents,omitzero"`
}

type SpawnTreeLoadParams struct {
	Profile *string `json:"profile,omitzero"`
	Path    string  `json:"path"`
}

type SpawnTreeListResult struct {
	Entries []SpawnTreeEntry `json:"entries"`
}

type SpawnTreeEntry struct {
	Path       string   `json:"path"`
	SessionID  *string  `json:"session_id,omitzero"`
	StartedAt  *float64 `json:"started_at,omitzero"`
	FinishedAt *float64 `json:"finished_at,omitzero"`
	Label      string   `json:"label,omitzero"`
	Count      int64    `json:"count,omitzero"`
}

type SpawnTreeListParams struct {
	Profile      *string `json:"profile,omitzero"`
	SessionID    *string `json:"session_id,omitzero"`
	CrossSession bool    `json:"cross_session,omitzero"`
	Limit        *int64  `json:"limit,omitzero"`
}

type SpawnTreeSaveResult struct {
	Path      string `json:"path"`
	SessionID string `json:"session_id"`
}

type SpawnTreeSaveParams struct {
	Profile    *string             `json:"profile,omitzero"`
	Subagents  []SpawnTreeSubagent `json:"subagents"`
	SessionID  *string             `json:"session_id,omitzero"`
	StartedAt  *float64            `json:"started_at,omitzero"`
	FinishedAt *float64            `json:"finished_at,omitzero"`
	Label      *string             `json:"label,omitzero"`
}

type SessionCompressResult struct {
	Status         *string             `json:"status,omitzero"`
	Removed        *int64              `json:"removed,omitzero"`
	BeforeMessages *int64              `json:"before_messages,omitzero"`
	AfterMessages  *int64              `json:"after_messages,omitzero"`
	BeforeTokens   *int64              `json:"before_tokens,omitzero"`
	AfterTokens    *int64              `json:"after_tokens,omitzero"`
	Summary        *CompressionSummary `json:"summary,omitzero"`
	Usage          *Usage              `json:"usage,omitzero"`
	Info           *SessionInfo        `json:"info,omitzero"`
	Messages       []TranscriptMessage `json:"messages,omitzero"`
	Compressed     *bool               `json:"compressed,omitzero"`
	LockHeld       *bool               `json:"lock_held,omitzero"`
	Message        *string             `json:"message,omitzero"`
}

type TranscriptMessage struct {
	Role            string           `json:"role"`
	Text            *string          `json:"text,omitzero"`
	Content         any              `json:"content,omitzero"`
	ToolCallID      *string          `json:"tool_call_id,omitzero"`
	Timestamp       *float64         `json:"timestamp,omitzero"`
	RowID           *int64           `json:"row_id,omitzero"`
	DisplayKind     *string          `json:"display_kind,omitzero"`
	DisplayMetadata *DisplayMetadata `json:"display_metadata,omitzero"`
	Name            *string          `json:"name,omitzero"`
	Context         *string          `json:"context,omitzero"`
	Args            map[string]any   `json:"args,omitzero"`
	Labels          []ToolLabel      `json:"labels,omitzero"`
	Reasoning       *string          `json:"reasoning,omitzero"`
	ToolName        *string          `json:"tool_name,omitzero"`
	IsError         *bool            `json:"is_error,omitzero"`
	Refused         *bool            `json:"refused,omitzero"`
	Error           *string          `json:"error,omitzero"`
	// What a tool call printed, how long it ran, the diff of an edit and the image it made or looked at, as its tool.complete carried them
	ResultText *string  `json:"result_text,omitzero"`
	DurationS  *float64 `json:"duration_s,omitzero"`
	InlineDiff *string  `json:"inline_diff,omitzero"`
	ImagePath  *string  `json:"image_path,omitzero"`
	// What a shell line a person ran printed and its exit status, on a shell row
	Shell    *ShellExecResult `json:"shell,omitzero"`
	Archived *bool            `json:"archived,omitzero"`
	Extra    map[string]any   `json:"-" wire:"extra"`
}

// MCP server of the session with its transport, tool count, connection state and sampling metrics
type McpServerStatus struct {
	Name      string           `json:"name"`
	Transport string           `json:"transport"`
	Tools     int64            `json:"tools"`
	Connected bool             `json:"connected"`
	Disabled  bool             `json:"disabled"`
	Status    MCPRuntimeStatus `json:"status"`
	Error     *string          `json:"error,omitzero"`
	Sampling  map[string]any   `json:"sampling,omitzero"`
}

type Usage struct {
	Model            string   `json:"model,omitzero"`
	Input            int64    `json:"input,omitzero"`
	Output           int64    `json:"output,omitzero"`
	Reasoning        int64    `json:"reasoning,omitzero"`
	Prompt           int64    `json:"prompt,omitzero"`
	Completion       int64    `json:"completion,omitzero"`
	Total            int64    `json:"total,omitzero"`
	Calls            int64    `json:"calls,omitzero"`
	Compressions     *int64   `json:"compressions,omitzero"`
	ContextUsed      *int64   `json:"context_used,omitzero"`
	ContextMax       *int64   `json:"context_max,omitzero"`
	ContextPercent   *int64   `json:"context_percent,omitzero"`
	ContextSource    *string  `json:"context_source,omitzero"`
	ContextEstimated *bool    `json:"context_estimated,omitzero"`
	CacheHitPct      *int64   `json:"cache_hit_pct,omitzero"`
	CacheRead        *int64   `json:"cache_read,omitzero"`
	CacheWrite       *int64   `json:"cache_write,omitzero"`
	AvgLatencyS      *float64 `json:"avg_latency_s,omitzero"`
	AvgTPS           *float64 `json:"avg_tps,omitzero"`
	ActiveSubagents  *int64   `json:"active_subagents,omitzero"`
	CostUsd          *float64 `json:"cost_usd,omitzero"`
	CostStatus       *string  `json:"cost_status,omitzero"`
	// Rate limit windows the provider's last answer carried, absent before any answer carried them
	RateLimit map[string]any `json:"rate_limit,omitzero"`
}

type ProjectRef struct {
	ID          string  `json:"id"`
	Slug        string  `json:"slug"`
	Name        string  `json:"name"`
	PrimaryPath *string `json:"primary_path,omitzero"`
}

type CompressionSummary struct {
	Noop             bool    `json:"noop,omitzero"`
	Aborted          bool    `json:"aborted,omitzero"`
	RefusedWouldGrow *bool   `json:"refused_would_grow,omitzero"`
	FallbackUsed     *bool   `json:"fallback_used,omitzero"`
	Headline         string  `json:"headline,omitzero"`
	TokenLine        string  `json:"token_line,omitzero"`
	Note             *string `json:"note,omitzero"`
}

type SessionCompressParams struct {
	SessionID  string  `json:"session_id"`
	Profile    *string `json:"profile,omitzero"`
	FocusTopic *string `json:"focus_topic,omitzero"`
}

type SessionContextBreakdownResult struct {
	Categories       []ContextCategory   `json:"categories"`
	ContextMax       int64               `json:"context_max"`
	ContextPercent   int64               `json:"context_percent"`
	ContextUsed      int64               `json:"context_used"`
	EstimatedTotal   int64               `json:"estimated_total"`
	ContextEstimated bool                `json:"context_estimated"`
	ContextSource    string              `json:"context_source"`
	Model            string              `json:"model"`
	ContextFiles     []ContextFileSource `json:"context_files,omitzero"`
}

type ContextFileSource struct {
	Label     string `json:"label"`
	Path      string `json:"path"`
	Chars     int64  `json:"chars"`
	EstTokens int64  `json:"est_tokens"`
	Loaded    bool   `json:"loaded"`
	Status    string `json:"status"`
}

type ContextCategory struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Tokens int64  `json:"tokens"`
}

type SessionContextBreakdownParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
}

type SessionUsageParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
}

type SessionHistoryResult struct {
	Count    int64               `json:"count"`
	Messages []TranscriptMessage `json:"messages"`
}

type SessionHistoryParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
}

type SessionStatusResult struct {
	Output string `json:"output"`
}

type SessionStatusParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
}

type SessionSaveResult struct {
	File *string `json:"file,omitzero"`
}

type SessionSaveParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
}

type SessionUndoResult struct {
	Removed  int64             `json:"removed"`
	UndoHint *UndoRollbackHint `json:"undo_hint,omitzero"`
}

// Why a client drops the last exchange, a retry sending the dropped prompt again
type UndoIntent string

func (UndoIntent) Values() []string { return []string{"undo", "retry"} }

type SessionUndoParams struct {
	SessionID string     `json:"session_id"`
	Profile   *string    `json:"profile,omitzero"`
	Intent    UndoIntent `json:"intent,omitzero"`
}

type SessionBranchResult struct {
	SessionID       string              `json:"session_id"`
	StoredSessionID string              `json:"stored_session_id"`
	Title           string              `json:"title"`
	Parent          string              `json:"parent"`
	MessageCount    int64               `json:"message_count"`
	Messages        []TranscriptMessage `json:"messages"`
	Info            SessionInfo         `json:"info"`
}

type SessionBranchParams struct {
	SessionID      string  `json:"session_id"`
	Profile        *string `json:"profile,omitzero"`
	Name           *string `json:"name,omitzero"`
	Count          *int64  `json:"count,omitzero"`
	IdempotencyKey *string `json:"idempotency_key,omitzero"`
}

type SessionCloseResult struct {
	Closed   bool     `json:"closed"`
	Messages []string `json:"messages,omitzero"`
}

type SessionCloseParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
}

type SessionWorkspaceMoveResult struct {
	Cwd         string  `json:"cwd"`
	Branch      *string `json:"branch,omitzero"`
	GitRepoRoot *string `json:"git_repo_root,omitzero"`
}

type SessionWorkspaceMoveParams struct {
	Profile    *string `json:"profile,omitzero"`
	SessionKey string  `json:"session_key"`
	Cwd        string  `json:"cwd"`
}

type SessionTitleResult struct {
	Title      string  `json:"title"`
	SessionKey *string `json:"session_key,omitzero"`
	Pending    *bool   `json:"pending,omitzero"`
}

type SessionTitleParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
	Title     *string `json:"title,omitzero"`
}

type SessionDeleteResult struct {
	Deleted string `json:"deleted"`
}

type SessionDeleteParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
}

type SessionActiveListResult struct {
	Sessions []SessionActiveItem `json:"sessions"`
}

type LiveSessionStatus string

func (LiveSessionStatus) Values() []string {
	return []string{"idle", "starting", "waiting", "working", "streaming", "resuming"}
}

type SessionActiveItem struct {
	Current      bool              `json:"current"`
	ID           string            `json:"id"`
	LastActive   float64           `json:"last_active"`
	MessageCount int64             `json:"message_count"`
	Model        string            `json:"model"`
	Preview      string            `json:"preview"`
	SessionKey   string            `json:"session_key"`
	Source       *string           `json:"source,omitzero"`
	StartedAt    float64           `json:"started_at"`
	Status       LiveSessionStatus `json:"status"`
	Title        string            `json:"title"`
}

type SessionActiveListParams struct {
	Profile          *string `json:"profile,omitzero"`
	CurrentSessionID *string `json:"current_session_id,omitzero"`
}

type SessionMostRecentResult struct {
	SessionID *string  `json:"session_id"`
	Title     *string  `json:"title,omitzero"`
	StartedAt *float64 `json:"started_at,omitzero"`
	Source    *string  `json:"source,omitzero"`
}

type SessionMostRecentParams struct {
	Profile *string `json:"profile,omitzero"`
}

type SessionListResult struct {
	Sessions []SessionListRow `json:"sessions"`
}

type SessionListRow struct {
	ID               string  `json:"id"`
	ResolvedID       *string `json:"resolved_id,omitzero"`
	Title            string  `json:"title,omitzero"`
	Preview          string  `json:"preview,omitzero"`
	StartedAt        float64 `json:"started_at,omitzero"`
	MessageCount     int64   `json:"message_count,omitzero"`
	LiveMessageCount *int64  `json:"live_message_count,omitzero"`
	Source           string  `json:"source,omitzero"`
}

type SessionListParams struct {
	Profile       *string `json:"profile,omitzero"`
	Title         *string `json:"title,omitzero"`
	Limit         *int64  `json:"limit,omitzero"`
	IncludeHidden bool    `json:"include_hidden,omitzero"`
}

type SessionActivateResult = SessionResumeResult

type AutoContinue struct {
	Attempt       int64   `json:"attempt"`
	InterruptedAt float64 `json:"interrupted_at"`
}

type OpenRequestEntry struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

type PendingApproval struct {
	RequestID      *string  `json:"request_id,omitzero"`
	Command        *string  `json:"command,omitzero"`
	Description    *string  `json:"description,omitzero"`
	PatternKey     *string  `json:"pattern_key,omitzero"`
	PatternKeys    []string `json:"pattern_keys,omitzero"`
	AllowPermanent *bool    `json:"allow_permanent,omitzero"`
	AllowSession   *bool    `json:"allow_session,omitzero"`
	SmartDenied    *bool    `json:"smart_denied,omitzero"`
	Choices        []string `json:"choices,omitzero"`
	ToolName       *string  `json:"tool_name,omitzero"`
}

type QueuedPrompt struct {
	User string `json:"user"`
}

type InflightTurn struct {
	Assistant         string           `json:"assistant,omitzero"`
	Streaming         bool             `json:"streaming,omitzero"`
	User              string           `json:"user,omitzero"`
	DisplayKind       *string          `json:"display_kind,omitzero"`
	DisplayMetadata   *DisplayMetadata `json:"display_metadata,omitzero"`
	Corrections       []string         `json:"corrections,omitzero"`
	CorrectionOffsets []int64          `json:"correction_offsets,omitzero"`
	Error             *string          `json:"error,omitzero"`
	Status            *string          `json:"status,omitzero"`
	Recoverable       *bool            `json:"recoverable,omitzero"`
	ErrorSurface      map[string]any   `json:"error_surface,omitzero"`
}

type SessionActivateParams struct {
	SessionID    string  `json:"session_id"`
	Profile      *string `json:"profile,omitzero"`
	Cols         *int64  `json:"cols,omitzero"`
	OmitMessages bool    `json:"omit_messages,omitzero"`
}

type SessionResumeResult struct {
	SessionID         string                    `json:"session_id"`
	MessageCount      int64                     `json:"message_count"`
	Messages          []TranscriptMessage       `json:"messages"`
	Info              SessionInfo               `json:"info"`
	StoredSessionID   *string                   `json:"stored_session_id,omitzero"`
	Resumed           *string                   `json:"resumed,omitzero"`
	SessionKey        *string                   `json:"session_key,omitzero"`
	MessagesOmitted   *bool                     `json:"messages_omitted,omitzero"`
	Hydrating         *bool                     `json:"hydrating,omitzero"`
	Running           *bool                     `json:"running,omitzero"`
	TurnStartedAt     *float64                  `json:"turn_started_at,omitzero"`
	StartedAt         *float64                  `json:"started_at,omitzero"`
	Status            *string                   `json:"status,omitzero"`
	Inflight          *InflightTurn             `json:"inflight,omitzero"`
	Queued            *QueuedPrompt             `json:"queued,omitzero"`
	PendingApproval   *PendingApproval          `json:"pending_approval,omitzero"`
	OpenRequests      []OpenRequestEntry        `json:"open_requests,omitzero"`
	PendingConnection *ConnectionRequestPayload `json:"pending_connection,omitzero"`
	TodoState         *TodoState                `json:"todo_state,omitzero"`
	AutoContinue      *AutoContinue             `json:"auto_continue,omitzero"`
}

type SessionResumeParams struct {
	SessionID         string  `json:"session_id"`
	Profile           *string `json:"profile,omitzero"`
	Cols              *int64  `json:"cols,omitzero"`
	Source            *string `json:"source,omitzero"`
	Lazy              bool    `json:"lazy,omitzero"`
	DeferHistory      bool    `json:"defer_history,omitzero"`
	OmitMessages      bool    `json:"omit_messages,omitzero"`
	EagerBuild        bool    `json:"eager_build,omitzero"`
	CloseOnDisconnect bool    `json:"close_on_disconnect,omitzero"`
	InlineImages      *bool   `json:"inline_images,omitzero"`
}

type SessionCreateResult struct {
	SessionID       string              `json:"session_id"`
	StoredSessionID string              `json:"stored_session_id"`
	MessageCount    int64               `json:"message_count"`
	Messages        []TranscriptMessage `json:"messages"`
	Info            SessionInfo         `json:"info"`
}

type SessionCreateParams struct {
	Profile             *string       `json:"profile,omitzero"`
	Cols                *int64        `json:"cols,omitzero"`
	Source              *string       `json:"source,omitzero"`
	Cwd                 *string       `json:"cwd,omitzero"`
	CwdExplicit         *bool         `json:"cwd_explicit,omitzero"`
	Messages            []SeedMessage `json:"messages,omitzero"`
	ParentSessionID     *string       `json:"parent_session_id,omitzero"`
	Title               *string       `json:"title,omitzero"`
	Model               *string       `json:"model,omitzero"`
	Provider            *string       `json:"provider,omitzero"`
	ReasoningEffort     *string       `json:"reasoning_effort,omitzero"`
	Fast                *bool         `json:"fast,omitzero"`
	ServiceTier         *string       `json:"service_tier,omitzero"`
	CloseOnDisconnect   bool          `json:"close_on_disconnect,omitzero"`
	Hidden              bool          `json:"hidden,omitzero"`
	RoomPlumbing        bool          `json:"room_plumbing,omitzero"`
	FollowProfileConfig bool          `json:"follow_profile_config,omitzero"`
	IdempotencyKey      *string       `json:"idempotency_key,omitzero"`
	// Session whose login choices, every provider's pin and exhaustion reaction, the new session starts with
	LoginsFrom *string `json:"logins_from,omitzero"`
}

type SeedMessage struct {
	Role        string  `json:"role"`
	Content     *string `json:"content,omitzero"`
	Text        *string `json:"text,omitzero"`
	DisplayKind *string `json:"display_kind,omitzero"`
}

type SessionControlUpdatePayload struct {
	Control SessionControlSnapshot `json:"control"`
}

type SessionControlSnapshot struct {
	Goal      *GoalSnapshot      `json:"goal"`
	Loop      *LoopSnapshot      `json:"loop"`
	Heartbeat *HeartbeatSnapshot `json:"heartbeat"`
	Revision  string             `json:"revision"`
	UpdatedAt float64            `json:"updated_at"`
}

type HeartbeatSnapshot struct {
	Prompt          string  `json:"prompt"`
	Status          string  `json:"status"`
	IntervalSeconds int64   `json:"interval_seconds"`
	CreatedAt       float64 `json:"created_at"`
	LastFiredAt     float64 `json:"last_fired_at"`
	FireCount       int64   `json:"fire_count"`
}

type LoopSnapshot struct {
	Prompt           string  `json:"prompt"`
	Status           string  `json:"status"`
	Mode             string  `json:"mode"`
	IntervalSeconds  float64 `json:"interval_seconds"`
	CurrentDelay     float64 `json:"current_delay"`
	Times            int64   `json:"times"`
	Until            string  `json:"until"`
	MaxTicks         int64   `json:"max_ticks"`
	TicksFired       int64   `json:"ticks_fired"`
	CreatedAt        float64 `json:"created_at"`
	LastFiredAt      float64 `json:"last_fired_at"`
	NextDueAt        float64 `json:"next_due_at"`
	AwaitingResponse bool    `json:"awaiting_response"`
	DeferredByGoal   bool    `json:"deferred_by_goal"`
	PausedReason     *string `json:"paused_reason,omitzero"`
	LastStopReason   *string `json:"last_stop_reason,omitzero"`
}

type GoalSnapshot struct {
	Title        string               `json:"title"`
	Status       string               `json:"status"`
	TurnsUsed    int64                `json:"turns_used"`
	MaxTurns     int64                `json:"max_turns"`
	Contract     GoalContractSnapshot `json:"contract"`
	Subgoals     []string             `json:"subgoals"`
	Gates        []GoalGateSnapshot   `json:"gates"`
	CreatedAt    *float64             `json:"created_at,omitzero"`
	UpdatedAt    *float64             `json:"updated_at,omitzero"`
	PausedReason *string              `json:"paused_reason,omitzero"`
	LastVerdict  *string              `json:"last_verdict,omitzero"`
	LastReason   *string              `json:"last_reason,omitzero"`
	WaitBarrier  *WaitBarrier         `json:"wait_barrier,omitzero"`
}

type GoalGateSnapshot struct {
	Command        string `json:"command"`
	TimeoutSeconds int64  `json:"timeout_seconds"`
	MaxRetries     int64  `json:"max_retries"`
	Attempts       int64  `json:"attempts"`
	LastExitCode   *int64 `json:"last_exit_code,omitzero"`
}

type GoalContractSnapshot struct {
	Outcome      string `json:"outcome,omitzero"`
	Verification string `json:"verification,omitzero"`
	Constraints  string `json:"constraints,omitzero"`
	Boundaries   string `json:"boundaries,omitzero"`
	StopWhen     string `json:"stop_when,omitzero"`
}

type SessionReclaimedPayload struct {
	SessionID       string `json:"session_id"`
	StoredSessionID string `json:"stored_session_id"`
	Reason          string `json:"reason"`
}

type ResumePhaseStatus string

func (ResumePhaseStatus) Values() []string { return []string{"loading", "complete", "failed"} }

type SessionResumeProgressPayload struct {
	Phase        string            `json:"phase"`
	Status       ResumePhaseStatus `json:"status"`
	MessageCount *int64            `json:"message_count,omitzero"`
	Message      *string           `json:"message,omitzero"`
}

type SessionTitlePayload struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
}

type SessionUsagePayload struct {
	Usage               *Usage         `json:"usage,omitzero"`
	InputTokens         *int64         `json:"input_tokens,omitzero"`
	OutputTokens        *int64         `json:"output_tokens,omitzero"`
	CacheReadTokens     *int64         `json:"cache_read_tokens,omitzero"`
	CacheWriteTokens    *int64         `json:"cache_write_tokens,omitzero"`
	PromptTokens        *int64         `json:"prompt_tokens,omitzero"`
	CompletionTokens    *int64         `json:"completion_tokens,omitzero"`
	TotalTokens         *int64         `json:"total_tokens,omitzero"`
	ReasoningTokens     *int64         `json:"reasoning_tokens,omitzero"`
	APICalls            *int64         `json:"api_calls,omitzero"`
	LastPromptTokens    *int64         `json:"last_prompt_tokens,omitzero"`
	ContextTokens       *int64         `json:"context_tokens,omitzero"`
	ContextLength       *int64         `json:"context_length,omitzero"`
	ConfigContextLength *int64         `json:"config_context_length,omitzero"`
	ContextSource       *string        `json:"context_source,omitzero"`
	CompressionCount    *int64         `json:"compression_count,omitzero"`
	AvgLatencyS         *float64       `json:"avg_latency_s,omitzero"`
	TokensPerS          *float64       `json:"tokens_per_s,omitzero"`
	LastTurnTokens      *int64         `json:"last_turn_tokens,omitzero"`
	RateLimit           map[string]any `json:"rate_limit,omitzero"`
	ActiveSubagents     *int64         `json:"active_subagents,omitzero"`
	Model               *string        `json:"model,omitzero"`
	Provider            *string        `json:"provider,omitzero"`
	FallbackModel       *string        `json:"fallback_model,omitzero"`
}

type GatewayReadyPayload struct {
	Skin         *SkinPayloadModel `json:"skin,omitzero"`
	ChangeEvents bool              `json:"change_events"`
	ReplayEpoch  string            `json:"replay_epoch"`
	Heartbeat    *bool             `json:"heartbeat,omitzero"`
	// Go build ID of the service
	Build string `json:"build"`
}

type SkinPayloadModel struct {
	Name string `json:"name,omitzero"`
	// builtin for a skin icebath ships unmodified, user for one read from the home skins folder
	Source      string            `json:"source,omitzero"`
	Description string            `json:"description,omitzero"`
	Colors      map[string]string `json:"colors,omitzero"`
	LightColors map[string]string `json:"light_colors,omitzero"`
	DarkColors  map[string]string `json:"dark_colors,omitzero"`
	Branding    map[string]string `json:"branding,omitzero"`
	BannerLogo  string            `json:"banner_logo,omitzero"`
	BannerHero  string            `json:"banner_hero,omitzero"`
	ToolPrefix  string            `json:"tool_prefix,omitzero"`
	HelpHeader  string            `json:"help_header,omitzero"`
}

type SessionControlReadResult struct {
	Control SessionControlSnapshot `json:"control"`
}

type SessionControlReadParams struct {
	Profile   *string `json:"profile,omitzero"`
	SessionID string  `json:"session_id"`
}

type CanonicalListResultScope string

func (CanonicalListResultScope) Values() []string { return []string{"stored", "live"} }

type CanonicalListResult struct {
	Sessions []CanonicalSessionListRow `json:"sessions"`
	Scope    CanonicalListResultScope  `json:"scope"`
}

type CanonicalSessionListRow struct {
	SessionID    string   `json:"session_id"`
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Source       *string  `json:"source"`
	StartedAt    *float64 `json:"started_at"`
	MessageCount int64    `json:"message_count"`
	Running      bool     `json:"running"`
	Model        *string  `json:"model,omitzero"`
	ResolvedID   *string  `json:"resolved_id,omitzero"`
	RootTitle    *string  `json:"root_title,omitzero"`
	// Opening words of the session's last exchange
	Preview  *string `json:"preview,omitzero"`
	Pinned   bool    `json:"pinned,omitzero"`
	Archived bool    `json:"archived,omitzero"`
	// Last activity in epoch seconds, on stored rows
	LastActive *float64 `json:"last_active,omitzero"`
}

type CanonicalListParamsScope string

func (CanonicalListParamsScope) Values() []string { return []string{"live", "stored"} }

type CanonicalListParams struct {
	Profile       *string                   `json:"profile,omitzero"`
	Scope         *CanonicalListParamsScope `json:"scope,omitzero"`
	Limit         *int64                    `json:"limit,omitzero"`
	Title         *string                   `json:"title,omitzero"`
	IncludeHidden bool                      `json:"include_hidden,omitzero"`
	// Stored scope only, archived sessions listed too
	IncludeArchived bool `json:"include_archived,omitzero"`
}

type CanonicalSnapshot struct {
	SessionID           string                      `json:"session_id"`
	StoredSessionID     string                      `json:"stored_session_id"`
	Messages            []TranscriptMessage         `json:"messages"`
	MessagesOmitted     *bool                       `json:"messages_omitted,omitzero"`
	DisplayMessages     []TranscriptMessage         `json:"display_messages,omitzero"`
	MessageCount        int64                       `json:"message_count"`
	Running             bool                        `json:"running"`
	AuthorityEpoch      int64                       `json:"authority_epoch"`
	ReplayEpoch         string                      `json:"replay_epoch"`
	LastSequence        int64                       `json:"last_sequence"`
	SubscriptionID      string                      `json:"subscription_id"`
	Revision            int64                       `json:"revision"`
	ExecutionGeneration int64                       `json:"execution_generation"`
	Pending             []CanonicalPendingAdmission `json:"pending"`
	Prompts             []CanonicalPendingPrompt    `json:"prompts"`
	Info                SessionInfo                 `json:"info"`
	PendingConnection   *ConnectionRequestPayload   `json:"pending_connection,omitzero"`
	TodoState           *TodoState                  `json:"todo_state,omitzero"`
	OpenRequests        []OpenRequestEntry          `json:"open_requests,omitzero"`
	// Set when the resume named a replay cursor: true when the events after it were replayed, false when the transcript came back instead
	Replayed *bool `json:"replayed,omitzero"`
}

type CanonicalPendingPromptKind string

func (CanonicalPendingPromptKind) Values() []string { return []string{"approval", "clarify"} }

type CanonicalPendingPrompt struct {
	Kind                CanonicalPendingPromptKind `json:"kind"`
	PromptID            string                     `json:"prompt_id"`
	ExecutionGeneration int64                      `json:"execution_generation"`
	Choices             []string                   `json:"choices"`
	Command             *string                    `json:"command,omitzero"`
	Description         *string                    `json:"description,omitzero"`
	Edit                map[string]any             `json:"edit,omitzero"`
	OpenURL             *string                    `json:"open_url,omitzero"`
	Question            *string                    `json:"question,omitzero"`
	MultiSelect         *bool                      `json:"multi_select,omitzero"`
	TimeoutS            *int64                     `json:"timeout_s,omitzero"`
}

type CanonicalPendingAdmissionStatus string

func (CanonicalPendingAdmissionStatus) Values() []string {
	return []string{"queued", "started", "unknown", "terminal"}
}

type CanonicalPendingAdmissionControls string

func (CanonicalPendingAdmissionControls) Values() []string { return []string{"steer", "replace"} }

type CanonicalPendingAdmission struct {
	AdmissionID         string                              `json:"admission_id"`
	Ref                 CanonicalSessionRef                 `json:"ref"`
	Sequence            int64                               `json:"sequence"`
	Status              CanonicalPendingAdmissionStatus     `json:"status"`
	Outcome             *string                             `json:"outcome"`
	AuthorityEpoch      int64                               `json:"authority_epoch"`
	ExecutionGeneration *int64                              `json:"execution_generation"`
	InputID             string                              `json:"input_id"`
	Text                string                              `json:"text"`
	DisplayKind         *string                             `json:"display_kind,omitzero"`
	DisplayMetadata     *DisplayMetadata                    `json:"display_metadata,omitzero"`
	Controls            []CanonicalPendingAdmissionControls `json:"controls,omitzero"`
	Steering            *bool                               `json:"steering,omitzero"`
}

type CanonicalSessionRef struct {
	ProfileID string `json:"profile_id"`
	SessionID string `json:"session_id"`
}

type CanonicalResumeParamsLatest string

func (CanonicalResumeParamsLatest) Values() []string { return []string{"cli", "tui"} }

type CanonicalResumeParams struct {
	Profile           *string                      `json:"profile,omitzero"`
	SessionID         string                       `json:"session_id,omitzero"`
	Title             *string                      `json:"title,omitzero"`
	Latest            *CanonicalResumeParamsLatest `json:"latest,omitzero"`
	Workspace         *string                      `json:"workspace,omitzero"`
	Source            *string                      `json:"source,omitzero"`
	Editor            map[string]any               `json:"editor,omitzero"`
	APIKey            *string                      `json:"api_key,omitzero"`
	DeferHistory      bool                         `json:"defer_history,omitzero"`
	OmitMessages      bool                         `json:"omit_messages,omitzero"`
	Cols              *int64                       `json:"cols,omitzero"`
	DisplayProjection bool                         `json:"display_projection,omitzero"`
	// Replays the events after this cursor instead of the whole transcript, the transcript coming back when the cursor fell out of the window
	ReplayFrom *ReplayCursor `json:"replay_from,omitzero"`
}

// Replay epoch and last sequence a client saw of one session
type ReplayCursor struct {
	Epoch    string `json:"epoch"`
	Sequence int64  `json:"sequence"`
}

type SessionRevisionParams struct {
	SessionID string `json:"session_id"`
}

// Fences of a session read without attaching to it
type SessionRevisionResult struct {
	SessionID           string `json:"session_id"`
	Revision            int64  `json:"revision"`
	ExecutionGeneration int64  `json:"execution_generation"`
	Running             bool   `json:"running"`
	AuthorityEpoch      int64  `json:"authority_epoch"`
}

type CanonicalCreateParams struct {
	Profile             *string           `json:"profile,omitzero"`
	RequestID           *string           `json:"request_id,omitzero"`
	Source              *string           `json:"source,omitzero"`
	Cwd                 *string           `json:"cwd,omitzero"`
	RowCwd              *string           `json:"row_cwd,omitzero"`
	Model               *string           `json:"model,omitzero"`
	Provider            *string           `json:"provider,omitzero"`
	BaseURL             *string           `json:"base_url,omitzero"`
	APIKey              *string           `json:"api_key,omitzero"`
	Toolsets            []string          `json:"toolsets,omitzero"`
	Reasoning           *string           `json:"reasoning,omitzero"`
	MaxTurns            *int64            `json:"max_turns,omitzero"`
	IgnoreRules         bool              `json:"ignore_rules,omitzero"`
	Yolo                bool              `json:"yolo,omitzero"`
	SafeMode            bool              `json:"safe_mode,omitzero"`
	IgnoreUserConfig    bool              `json:"ignore_user_config,omitzero"`
	Skills              []string          `json:"skills,omitzero"`
	Checkpoints         bool              `json:"checkpoints,omitzero"`
	AcceptHooks         bool              `json:"accept_hooks,omitzero"`
	PassSessionID       bool              `json:"pass_session_id,omitzero"`
	Editor              map[string]any    `json:"editor,omitzero"`
	Title               *string           `json:"title,omitzero"`
	Hidden              bool              `json:"hidden,omitzero"`
	FollowProfileConfig *bool             `json:"follow_profile_config,omitzero"`
	ServiceTier         *string           `json:"service_tier,omitzero"`
	CwdExplicit         *bool             `json:"cwd_explicit,omitzero"`
	RunBudget           *float64          `json:"run_budget,omitzero"`
	Verbose             bool              `json:"verbose,omitzero"`
	ClientEnv           map[string]string `json:"client_env,omitzero"`
	// Session whose login choices, every provider's pin and exhaustion reaction, the new session starts with
	LoginsFrom *string `json:"logins_from,omitzero"`
}

type RuntimeDescribeResult struct {
	InstanceID     string            `json:"instance_id"`
	ProfileID      string            `json:"profile_id"`
	AuthorityEpoch int64             `json:"authority_epoch"`
	Skin           *SkinPayloadModel `json:"skin,omitzero"`
	Build          string            `json:"build"`
}

type RuntimeDescribeParams struct {
}

type SessionDetachResult struct {
	SessionID      string `json:"session_id"`
	SubscriptionID string `json:"subscription_id"`
	Detached       bool   `json:"detached"`
}

type SessionDetachParams struct {
	SessionID      string  `json:"session_id"`
	SubscriptionID string  `json:"subscription_id"`
	EndReason      *string `json:"end_reason,omitzero"`
}

type SessionAutomationResult struct {
	State *string           `json:"state"`
	Value any               `json:"value,omitzero"`
	Error map[string]string `json:"error,omitzero"`
}

type SessionAutomationParamsKind string

func (SessionAutomationParamsKind) Values() []string { return []string{"goal", "loop", "heartbeat"} }

type SessionAutomationParams struct {
	SessionID string                      `json:"session_id"`
	Kind      SessionAutomationParamsKind `json:"kind"`
	Call      string                      `json:"call"`
	Args      []any                       `json:"args,omitzero"`
	Kwargs    map[string]any              `json:"kwargs,omitzero"`
}

type SessionExportResult struct {
	Format   string `json:"format"`
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

type SessionExportParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
	Format    *string `json:"format,omitzero"`
	Redact    bool    `json:"redact,omitzero"`
}

type SessionStats struct {
	InputTokens         int64          `json:"input_tokens"`
	OutputTokens        int64          `json:"output_tokens"`
	CacheReadTokens     int64          `json:"cache_read_tokens"`
	CacheWriteTokens    int64          `json:"cache_write_tokens"`
	PromptTokens        int64          `json:"prompt_tokens"`
	CompletionTokens    int64          `json:"completion_tokens"`
	TotalTokens         int64          `json:"total_tokens"`
	ReasoningTokens     int64          `json:"reasoning_tokens"`
	APICalls            int64          `json:"api_calls"`
	LastPromptTokens    int64          `json:"last_prompt_tokens"`
	ContextTokens       int64          `json:"context_tokens"`
	ContextLength       *int64         `json:"context_length,omitzero"`
	ConfigContextLength *int64         `json:"config_context_length,omitzero"`
	ContextSource       *string        `json:"context_source,omitzero"`
	CompressionCount    int64          `json:"compression_count"`
	AvgLatencyS         *float64       `json:"avg_latency_s,omitzero"`
	TokensPerS          *float64       `json:"tokens_per_s,omitzero"`
	LastTurnTokens      int64          `json:"last_turn_tokens"`
	RateLimit           map[string]any `json:"rate_limit,omitzero"`
	ActiveSubagents     int64          `json:"active_subagents"`
	Model               string         `json:"model"`
	Provider            string         `json:"provider"`
	FallbackModel       *string        `json:"fallback_model,omitzero"`
}

type SessionStatsParams struct {
	SessionID string `json:"session_id"`
}

type CanonicalSessionInfoParams struct {
	SessionID string `json:"session_id"`
}

// Session whose provider account limits are read
type SessionAccountUsageParams struct {
	SessionID string `json:"session_id"`
}

// Account limit lines of the provider a session runs on, empty when the provider reports none
type SessionAccountUsage struct {
	Lines []string `json:"lines"`
}

type SessionPruneResult struct {
	Deleted        int64  `json:"deleted"`
	RoutingDeleted *int64 `json:"routing_deleted,omitzero"`
	Skipped        *int64 `json:"skipped,omitzero"`
}

type SessionPruneParams struct {
	Filters         map[string]any `json:"filters,omitzero"`
	NeverActiveDays *float64       `json:"never_active_days,omitzero"`
}

type SessionMutateResult struct {
	SessionID           string              `json:"session_id,omitzero"`
	Operation           string              `json:"operation"`
	Revision            *int64              `json:"revision,omitzero"`
	ExecutionGeneration *int64              `json:"execution_generation,omitzero"`
	Status              *string             `json:"status,omitzero"`
	Title               *string             `json:"title,omitzero"`
	Archived            *bool               `json:"archived,omitzero"`
	BranchedSessionID   *string             `json:"branched_session_id,omitzero"`
	Model               *string             `json:"model,omitzero"`
	Provider            *string             `json:"provider,omitzero"`
	Effort              *string             `json:"effort,omitzero"`
	Note                *string             `json:"note,omitzero"`
	Confirm             *string             `json:"confirm,omitzero"`
	ConfirmMessage      *string             `json:"confirm_message,omitzero"`
	TargetModel         *string             `json:"target_model,omitzero"`
	Reason              *string             `json:"reason,omitzero"`
	Payload             *ModelPick          `json:"payload,omitzero"`
	CommittedModel      *string             `json:"committed_model,omitzero"`
	CommittedProvider   *string             `json:"committed_provider,omitzero"`
	RewoundCount        *int64              `json:"rewound_count,omitzero"`
	TargetMessage       *TranscriptMessage  `json:"target_message,omitzero"`
	Summary             *CompressionSummary `json:"summary,omitzero"`
	Lines               []string            `json:"lines,omitzero"`
	Extra               map[string]any      `json:"-" wire:"extra"`
}

// Model a deferred pick asked for, with its provider and reasoning effort
type ModelPick struct {
	Model     string  `json:"model"`
	Provider  *string `json:"provider,omitzero"`
	Reasoning *string `json:"reasoning,omitzero"`
}

// Replay buffer of a session overflowed, the client resumes to catch up
type SessionReplayGapPayload struct {
	ReplayEpoch string `json:"replay_epoch"`
	LatestSeq   int64  `json:"latest_seq"`
}

type SessionMutateParams struct {
	SessionID          string         `json:"session_id"`
	RequestID          string         `json:"request_id"`
	ExpectedRevision   int64          `json:"expected_revision"`
	Operation          string         `json:"operation"`
	Payload            map[string]any `json:"payload"`
	ExpectedGeneration *int64         `json:"expected_generation,omitzero"`
}

// Creates a session on the session authority and attaches to it
var CanonicalSessionCreate = NewCanonicalMethod[CanonicalCreateParams, CanonicalSnapshot]("session.create")

// Attaches to a stored or live session, by id, title or the latest of a source
var CanonicalSessionResume = NewCanonicalMethod[CanonicalResumeParams, CanonicalSnapshot]("session.resume")

// Revision and generation of a session read with no side effect, the fences a session.mutate checks
var CanonicalSessionRevision = NewCanonicalMethod[SessionRevisionParams, SessionRevisionResult]("session.revision")

// Lists the live or stored sessions of the profile
var CanonicalSessionList = NewCanonicalMethod[CanonicalListParams, CanonicalListResult]("session.list")

// Frozen launch policy and inventory of one local session
var CanonicalSessionInfo = NewCanonicalMethod[CanonicalSessionInfoParams, SessionInfo]("session.info")

// Cumulative token, context, latency and rate limit counters of a session
var CanonicalSessionStats = NewCanonicalMethod[SessionStatsParams, SessionStats]("session.stats")

// Account limits of the provider a session runs on, as the lines the usage command prints under the totals
var CanonicalSessionAccountUsage = NewCanonicalMethod[SessionAccountUsageParams, SessionAccountUsage]("session.account_usage")

// Revision fenced, retry safe session edit, its operation projection riding beside the receipt
var CanonicalSessionMutate = NewCanonicalMethod[SessionMutateParams, SessionMutateResult]("session.mutate")

// Deletes the ended sessions a prune selected, skipping busy ones, local operator only
var CanonicalSessionPrune = NewCanonicalMethod[SessionPruneParams, SessionPruneResult]("session.prune")

// Writes a session transcript to a file or returns it inline
var CanonicalSessionExport = NewCanonicalMethod[SessionExportParams, SessionExportResult]("session.export")

// Arms or reads the automation a viewer runs on a session
var CanonicalSessionAutomation = NewCanonicalMethod[SessionAutomationParams, SessionAutomationResult]("session.automation")

// Ends one subscription of the caller to a session
var CanonicalSessionDetach = NewCanonicalMethod[SessionDetachParams, SessionDetachResult]("session.detach")

// Session creation policy, protocol and capabilities of the runtime
var CanonicalRuntimeDescribe = NewCanonicalMethod[RuntimeDescribeParams, RuntimeDescribeResult]("runtime.describe")

// Creates a live session on the method table route
var SessionCreate = NewMethod[SessionCreateParams, SessionCreateResult]("session.create")

// Resumes a stored session by id or exact title on the method table route
var SessionResume = NewMethod[SessionResumeParams, SessionResumeResult]("session.resume")

// Switches to a live session the gateway already runs
var SessionActivate = NewMethod[SessionActivateParams, SessionActivateResult]("session.activate")

// Lists stored sessions, newest first
var SessionList = NewMethod[SessionListParams, SessionListResult]("session.list")

// Newest stored session of the profile
var SessionMostRecent = NewMethod[SessionMostRecentParams, SessionMostRecentResult]("session.most_recent")

// Live sessions of the gateway with their state, the current one marked
var SessionActiveList = NewMethod[SessionActiveListParams, SessionActiveListResult]("session.active_list")

// Deletes a stored session
var SessionDelete = NewMethod[SessionDeleteParams, SessionDeleteResult]("session.delete")

// Reads or sets a live session title, a title set before the first turn applied once it exists
var SessionTitle = NewMethod[SessionTitleParams, SessionTitleResult]("session.title")

// Moves a live session to another working directory
var SessionWorkspaceMove = NewMethod[SessionWorkspaceMoveParams, SessionWorkspaceMoveResult]("session.workspace.move")

// Closes a live session and returns its closing lines
var SessionClose = NewMethod[SessionCloseParams, SessionCloseResult]("session.close")

// Forks a live session into a new one
var SessionBranch = NewMethod[SessionBranchParams, SessionBranchResult]("session.branch")

// Removes the last exchange of a live session
var SessionUndo = NewMethod[SessionUndoParams, SessionUndoResult]("session.undo")

// Saves the transcript of a live session to a file
var SessionSave = NewMethod[SessionSaveParams, SessionSaveResult]("session.save")

// Status text of a live session
var SessionStatus = NewMethod[SessionStatusParams, SessionStatusResult]("session.status")

// Transcript of a live session
var SessionHistory = NewMethod[SessionHistoryParams, SessionHistoryResult]("session.history")

// Token, context and cost counters of a live session
var SessionUsage = NewMethod[SessionUsageParams, Usage]("session.usage")

// Context window of a live session by category and by context file
var SessionContextBreakdown = NewMethod[SessionContextBreakdownParams, SessionContextBreakdownResult]("session.context_breakdown")

// Compresses a live session's transcript, optionally around a focus topic
var SessionCompress = NewMethod[SessionCompressParams, SessionCompressResult]("session.compress")

// Saves a spawn tree snapshot of a session's subagents
var SpawnTreeSave = NewMethod[SpawnTreeSaveParams, SpawnTreeSaveResult]("spawn_tree.save")

// Lists saved spawn tree snapshots
var SpawnTreeList = NewMethod[SpawnTreeListParams, SpawnTreeListResult]("spawn_tree.list")

// Loads one saved spawn tree snapshot
var SpawnTreeLoad = NewMethod[SpawnTreeLoadParams, SpawnTreeLoadResult]("spawn_tree.load")

// Tells a live session the terminal width it renders for
var TerminalResize = NewMethod[TerminalResizeParams, TerminalResizeResult]("terminal.resize")

// Goal, loop and heartbeat state of a session
var SessionControlRead = NewMethod[SessionControlReadParams, SessionControlReadResult]("session.control.read")

// First frame of a connection, the skin, change events, replay epoch and session authority
var GatewayReadyEvent = NewEvent[GatewayReadyPayload]("gateway.ready")

// The active skin changed, its resolved colours and branding
var SkinChangedEvent = NewEvent[SkinPayloadModel]("skin.changed")

// Settings, inventory or live state of a session changed, the fields carried replacing the held ones
var SessionInfoEvent = NewEvent[SessionInfo]("session.info")

// Usage counters of a session after an API call or a turn
var SessionUsageEvent = NewEvent[SessionUsagePayload]("session.usage")

// Automatic titling renamed a session
var SessionTitleEvent = NewEvent[SessionTitlePayload]("session.title")

// Progress of a deferred resume loading the history
var SessionResumeProgressEvent = NewEvent[SessionResumeProgressPayload]("session.resume_progress")

// The gateway reclaimed a live session from its clients
var SessionReclaimedEvent = NewEvent[SessionReclaimedPayload]("session.reclaimed")

// A session edit committed or a deferred model pick settled, its projection riding beside the operation
var SessionUpdatedEvent = NewEvent[SessionMutateResult]("session.updated")

// The replay buffer of a session overflowed, the client resumes it
var SessionReplayGapEvent = NewEvent[SessionReplayGapPayload]("session.replay_gap")

// Persisted goal, loop or heartbeat state of a session changed
var SessionControlUpdateEvent = NewEvent[SessionControlUpdatePayload]("session.control.update")
