package config

import "gopkg.in/yaml.v3"

// Scheduled job section, the switches read as off only when written as false where the readers test for false
type CronConfig struct {
	CatchUpMissed           bool          `yaml:"catch_up_missed" coerce:"exact"`
	AllowAgentScheduling    bool          `yaml:"allow_agent_scheduling" coerce:"bool"`
	Preflight               bool          `yaml:"preflight" coerce:"exact"`
	Model                   string        `yaml:"model"`
	ModelProvider           string        `yaml:"model_provider"`
	Provider                string        `yaml:"provider"`
	Chronos                 ChronosConfig `yaml:"chronos"`
	WrapResponse            bool          `yaml:"wrap_response" coerce:"bool"`
	Delivery                CronDelivery  `yaml:"delivery"`
	MirrorDelivery          bool          `yaml:"mirror_delivery" coerce:"bool"`
	MaxParallelJobs         *int          `yaml:"max_parallel_jobs"`
	OutputRetention         int           `yaml:"output_retention"`
	ScriptTimeoutSeconds    float64       `yaml:"script_timeout_seconds"`
	SessionDBTimeoutSeconds float64       `yaml:"session_db_timeout_seconds"`
	MediaSendTimeoutSeconds float64       `yaml:"media_send_timeout_seconds"`
	RequireRestartSafeScope bool          `yaml:"require_restart_safe_scope" coerce:"bool"`
	FailureRepeatAlertHours float64       `yaml:"failure_repeat_alert_hours"`
	CleanupTimeoutSeconds   *float64      `yaml:"cleanup_timeout_seconds,omitempty"`
	RetryUnreachable        *bool         `yaml:"retry_unreachable,omitempty" coerce:"exact"`
	CompletedRetentionDays  *float64      `yaml:"completed_retention_days,omitempty"`
	MisfireGraceMinutes     *float64      `yaml:"misfire_grace_minutes,omitempty"`
	StandaloneSendTimeoutS  *int          `yaml:"standalone_send_timeout_seconds,omitempty"`
	rest                    rest
}

type cronFields CronConfig

// Decodes the mapping and keeps unknown keys
func (c *CronConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*cronFields)(c), &c.rest)
}

// Writes the mapping with unknown keys after the named ones
func (c CronConfig) MarshalYAML() (any, error) {
	return encodeMapping(cronFields(c), c.rest)
}

// Managed cron scheduler settings
type ChronosConfig struct {
	PortalURL        string `yaml:"portal_url"`
	CallbackURL      string `yaml:"callback_url"`
	ExpectedAudience string `yaml:"expected_audience"`
	NASJWKSURL       string `yaml:"nas_jwks_url"`
	rest             rest
}

type chronosFields ChronosConfig

// Decodes the mapping and keeps unknown keys
func (c *ChronosConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*chronosFields)(c), &c.rest)
}

// Writes the mapping with unknown keys after the named ones
func (c ChronosConfig) MarshalYAML() (any, error) {
	return encodeMapping(chronosFields(c), c.rest)
}

// Delivery behaviour of cron output through a live gateway adapter
type CronDelivery struct {
	Notify bool `yaml:"notify" coerce:"exact"`
	rest   rest
}

type cronDeliveryFields CronDelivery

// Decodes the mapping and keeps unknown keys
func (c *CronDelivery) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*cronDeliveryFields)(c), &c.rest)
}

// Writes the mapping with unknown keys after the named ones
func (c CronDelivery) MarshalYAML() (any, error) {
	return encodeMapping(cronDeliveryFields(c), c.rest)
}

// Kanban coordination section
type KanbanConfig struct {
	AutoSubscribeOnCreate       bool        `yaml:"auto_subscribe_on_create" coerce:"bool"`
	NotifyInGateway             bool        `yaml:"notify_in_gateway" coerce:"bool"`
	DispatchInGateway           bool        `yaml:"dispatch_in_gateway" coerce:"bool"`
	ReviewDispatch              bool        `yaml:"review_dispatch" coerce:"bool"`
	DispatchIntervalSeconds     float64     `yaml:"dispatch_interval_seconds"`
	FailureLimit                int         `yaml:"failure_limit"`
	WorkerLogRotateBytes        int64       `yaml:"worker_log_rotate_bytes"`
	WorkerLogBackupCount        int         `yaml:"worker_log_backup_count"`
	OrchestratorProfile         string      `yaml:"orchestrator_profile"`
	DefaultAssignee             string      `yaml:"default_assignee"`
	MaxInProgress               *int        `yaml:"max_in_progress"`
	MaxInProgressPerProfile     *int        `yaml:"max_in_progress_per_profile"`
	DispatchProfiles            *StringList `yaml:"dispatch_profiles"`
	AutoDecompose               bool        `yaml:"auto_decompose" coerce:"bool"`
	AutoDecomposePerTick        int         `yaml:"auto_decompose_per_tick"`
	DispatchStaleTimeoutSeconds float64     `yaml:"dispatch_stale_timeout_seconds"`
	ReconcileOrphans            bool        `yaml:"reconcile_orphans" coerce:"bool"`
	DoneSubRetentionDays        int         `yaml:"done_sub_retention_days"`
	MaxSpawn                    *int        `yaml:"max_spawn,omitempty"`
	AutoPromoteChildren         *bool       `yaml:"auto_promote_children,omitempty" coerce:"bool"`
	rest                        rest
}

type kanbanFields KanbanConfig

// Decodes the mapping and keeps unknown keys
func (k *KanbanConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*kanbanFields)(k), &k.rest)
}

// Writes the mapping with unknown keys after the named ones
func (k KanbanConfig) MarshalYAML() (any, error) {
	return encodeMapping(kanbanFields(k), k.rest)
}

// Bot Mode cross-connection relay section
type BotModeConfig struct {
	EnvelopeTTLSeconds float64 `yaml:"envelope_ttl_seconds"`
	TurnWaitSeconds    float64 `yaml:"turn_wait_seconds"`
	rest               rest
}

type botModeFields BotModeConfig

// Decodes the mapping and keeps unknown keys
func (b *BotModeConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*botModeFields)(b), &b.rest)
}

// Writes the mapping with unknown keys after the named ones
func (b BotModeConfig) MarshalYAML() (any, error) {
	return encodeMapping(botModeFields(b), b.rest)
}

// Programmatic tool call sandbox section, with the ignored kernel_mode key kept as written
type CodeExecutionConfig struct {
	Mode              CodeExecMode `yaml:"mode"`
	KernelIdleTimeout int          `yaml:"kernel_idle_timeout"`
	MaxSessionKernels int          `yaml:"max_session_kernels"`
	Timeout           *float64     `yaml:"timeout,omitempty"`
	MaxToolCalls      *int         `yaml:"max_tool_calls,omitempty"`
	KernelMode        string       `yaml:"kernel_mode,omitempty"`
	rest              rest
}

type codeExecutionFields CodeExecutionConfig

// Decodes the mapping and keeps unknown keys
func (c *CodeExecutionConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*codeExecutionFields)(c), &c.rest)
}

// Writes the mapping with unknown keys after the named ones
func (c CodeExecutionConfig) MarshalYAML() (any, error) {
	return encodeMapping(codeExecutionFields(c), c.rest)
}

// Where execute_code runs the user's scripts
type CodeExecMode string

// Code execution modes
const (
	CodeExecProject CodeExecMode = "project"
	CodeExecStrict  CodeExecMode = "strict"
)

// Accepted code execution modes
func CodeExecModes() []CodeExecMode {
	return []CodeExecMode{CodeExecProject, CodeExecStrict}
}

// Defaults of the cron section
func defaultCron() CronConfig {
	return CronConfig{
		CatchUpMissed:           true,
		Preflight:               true,
		Chronos:                 ChronosConfig{PortalURL: "https://portal.icebath.app"},
		WrapResponse:            true,
		Delivery:                CronDelivery{Notify: true},
		OutputRetention:         50,
		ScriptTimeoutSeconds:    3600,
		SessionDBTimeoutSeconds: 10,
		MediaSendTimeoutSeconds: 300,
		FailureRepeatAlertHours: 6,
	}
}

// Defaults of the kanban section
func defaultKanban() KanbanConfig {
	return KanbanConfig{
		AutoSubscribeOnCreate:       true,
		NotifyInGateway:             true,
		DispatchInGateway:           true,
		ReviewDispatch:              true,
		DispatchIntervalSeconds:     60,
		FailureLimit:                2,
		WorkerLogRotateBytes:        2 * 1024 * 1024,
		WorkerLogBackupCount:        1,
		AutoDecompose:               true,
		AutoDecomposePerTick:        3,
		DispatchStaleTimeoutSeconds: 14400,
		ReconcileOrphans:            true,
		DoneSubRetentionDays:        30,
	}
}

// Documentation of the cron, kanban, bot mode and code execution sections
func cronDocs() map[string]string {
	return map[string]string{
		"cron.catch_up_missed":                  "false skips recurring misses beyond the local grace window.",
		"cron.allow_agent_scheduling":           "Let cron-spawned agents use the cronjob toolset (the \"cron-librarian\" pattern). Off by default: policy-denied in cron context to prevent unattended scheduling loops. Jobs created this way are user-owned in the same flat jobs table. Interactive toolsets (messaging, clarify) stay denied in cron regardless.",
		"cron.preflight":                        "Pre-dispatch validation: before building any agent machinery, verify the provider API key resolves (unless a fallback chain exists), attached skills are ready, and delivery platforms are configured. Failure -> last_status=blocked_config, ONE alert, no LLM call. false = fail during the run instead.",
		"cron.model":                            "Default model for cron jobs (WHAT model runs). Fire-time resolution: per-job pin > cron.model > model.default (the main agent model). An unpinned job follows the main model on every run; cron.model decouples the whole fleet from chat. \"\" = fall through.",
		"cron.model_provider":                   "Inference provider paired with cron.model (NOT the scheduler provider below). \"\" = resolve from global config.",
		"cron.provider":                         "Cron SCHEDULER provider (WHEN a due job fires). \"\" = built-in in-process 60s ticker. Name an installed provider (plugins/cron_providers/<name>/ or $ICEBATH_HOME/plugins/<name>/), e.g. \"chronos\" (managed cron for scale-to-zero). An unknown or unavailable provider falls back to the built-in so cron never loses its trigger.",
		"cron.chronos":                          "Chronos settings, consulted only when provider == \"chronos\". All non-secret: the agent holds NO scheduler credentials (provisioning reuses the Icebath Portal token).",
		"cron.chronos.portal_url":               "Portal base URL that arms and cancels one-shots and mints the inbound fire JWT (used as the expected issuer).",
		"cron.chronos.callback_url":             "This agent's publicly reachable base URL; the portal POSTs {callback_url}/api/cron/fire. \"\" -> Chronos unavailable, the resolver falls back to the built-in ticker.",
		"cron.chronos.expected_audience":        "Expected JWT audience (e.g. \"agent:{instance_id}\").",
		"cron.chronos.nas_jwks_url":             "JWKS URL for verifying the fire JWT signature. \"\" -> the fire endpoint refuses all tokens (never an unsigned decode).",
		"cron.wrap_response":                    "Wrap delivered cron responses with a task-name header and \"The agent cannot see this message\" footer. false = clean output.",
		"cron.delivery":                         "Delivery behaviour for cron output sent through a live gateway adapter.",
		"cron.delivery.notify":                  "Mark cron deliveries FINAL so the platform pushes them (Telegram's \"important\" mode otherwise sends with disable_notification=true and briefs look undelivered). false = silent, no-push deliveries.",
		"cron.mirror_delivery":                  "Make cron deliveries CONTINUABLE (the user can reply to a brief with it in context). false keeps deliveries isolated to the job's session; per-job `attach_to_session` overrides. Thread-capable platforms (Telegram topics, Discord and Slack threads) get a seeded thread per job; DM-only platforms mirror the brief into the target DM session. Appended at a turn boundary, cached system prompt untouched. User-written bare platforms address home conversations, unlike `all` broadcast expansions, which do not gain mirror eligibility.",
		"cron.max_parallel_jobs":                "Max due jobs run in parallel per tick. null or 0 = unbounded (thread count only); 1 = serial. Env override: ICEBATH_CRON_MAX_PARALLEL.",
		"cron.output_retention":                 "Keep the N most recent .md output files per job; 0 or negative disables pruning (for externally managed cleanup).",
		"cron.script_timeout_seconds":           "Timeout (seconds) for a no-agent cron script. Env: ICEBATH_CRON_SCRIPT_TIMEOUT.",
		"cron.session_db_timeout_seconds":       "Timeout (seconds) for session database init inside cron jobs: a state.db open or migrate has no timeout of its own, and an unbounded hang wedges the job's dispatch guard forever. Env: ICEBATH_CRON_SESSION_DB_TIMEOUT. 0 = unlimited.",
		"cron.media_send_timeout_seconds":       "Timeout (seconds) per media attachment send during gateway delivery; large attachments (long TTS audio, big exports) need more than 30s. Env: ICEBATH_CRON_MEDIA_SEND_TIMEOUT.",
		"cron.require_restart_safe_scope":       "Managed systemd gateway with no user session (containers, no linger): false runs cron jobs as a direct external subprocess (warns once; no cgroup isolation), true fails closed with the enable-linger remedy. Kanban always requires a scope.",
		"cron.completed_retention_days":         "Days a completed one-shot job stays in the jobs file so its final status remains inspectable (default 7); 0 or negative keeps them forever.",
		"cron.misfire_grace_minutes":            "Minutes an overdue job may still fire late through the scheduler provider's catch-up sweep; 0 or negative disables the sweep.",
		"cron.standalone_send_timeout_seconds":  "Seconds one out-of-process cron delivery send may take before it is abandoned (default 60); 0 or negative uses the default.",
		"cron.failure_repeat_alert_hours":       "A job failing with the SAME error alerts once, then stays silent for this many hours before one reminder ping (the run is still recorded; `icebath cron incidents` shows it). A green run or a different error alerts again immediately; `icebath cron incidents ack` silences a signature for good. 0 = re-alert on every failing run.",
		"kanban":                                "Kanban multi-agent coordination. The dispatcher ticks every N seconds, reclaims stale claims, promotes dependency-satisfied pending tasks to ready, and fires `icebath -p <assignee> chat -q ...` per claimable task. Run ONE dispatcher per profile; two on the same kanban.db race for claims.",
		"kanban.auto_subscribe_on_create":       "Auto-subscribe the originating gateway or TUI session to completion and block events when kanban_create is called from a session with a persistent delivery channel. Disable for profiles that prefer explicit kanban_notify-subscribe calls per task.",
		"kanban.notify_in_gateway":              "Poll and deliver Kanban subscriptions from this gateway. Disable on profiles that do not own notification subscriptions to avoid an idle five-second board probe.",
		"kanban.dispatch_in_gateway":            "Run the dispatcher inside the gateway process (~300us per idle tick). false only if you run it as a separate unit or don't want the gateway spawning workers.",
		"kanban.review_dispatch":                "Auto-claim tasks in the review column and spawn the assigned profile with the bundled sdlc-review skill. Disable where every review is done manually from the dashboard.",
		"kanban.dispatch_interval_seconds":      "Seconds between dispatcher ticks. Lower = snappier pickup; higher = less SQL pressure.",
		"kanban.failure_limit":                  "Auto-block after this many consecutive non-success attempts (spawn_failed, timed_out, crashed) for the same task and profile. Reassignment resets the streak.",
		"kanban.worker_log_rotate_bytes":        "Worker stdout and stderr log rotation at spawn time (2 MiB + one backup). Raise to keep more early failure evidence from long-running workers.",
		"kanban.orchestrator_profile":           "Profile for the root or orchestration task after Triage decomposition; \"\" = default profile. Does not control the decomposer LLM path (see auxiliary.kanban_decomposer).",
		"kanban.default_assignee":               "Assignee when the orchestrator can't match one to an installed profile; \"\" = default profile. A task never ends up with assignee=null.",
		"kanban.max_in_progress":                "Global cap: a positive int = the HOST never has more than N tasks 'running' across all boards and both dispatch lanes (workers are OS processes sharing one machine's memory, so the cap bounds the machine, not each board). null = ~MemTotal / 512 MiB clamped to [2, 8]; where MemTotal is unreadable (macOS, Windows) null means no cap.",
		"kanban.max_in_progress_per_profile":    "Per-profile cap: a positive int = no single profile runs more than N workers even if the global caps allow; blocked tasks defer to the next tick. null = no per-profile cap. Useful when fan-out would saturate one profile's model, API quota or browser pool.",
		"kanban.dispatch_profiles":              "Per-home claim allowlist for boards shared across icebath homes: profile names this home's dispatcher may claim (list or comma-separated string). null = any existing profile is claimable. Set = fail-closed (an empty list claims nothing). Every home has a root profile named \"default\", so on a shared kanban.db every home can otherwise claim default-assigned cards.",
		"kanban.auto_decompose":                 "Auto-run the decomposer on Triage tasks every tick. false = manual via `icebath kanban decompose <id>` or the dashboard's Decompose button.",
		"kanban.auto_decompose_per_tick":        "Max triage tasks decomposed per tick, bounding the aux-LLM burst from a bulk load. Excess defers to the next tick.",
		"kanban.dispatch_stale_timeout_seconds": "Running tasks with no heartbeat for this many seconds are reclaimed to ready on the next tick; a still-running local worker is terminated first. 0 = off.",
		"kanban.reconcile_orphans":              "Each tick, requeue 'running' cards with broken claim bookkeeping (claim_lock or claim_expires NULL with a dead worker) that TTL, crash or stale recovery can't see. false keeps orphans frozen for manual forensics.",
		"kanban.done_sub_retention_days":        "Notify subscriptions survive `done` (completion is reversible) and are removed on archive. On boards that never archive, the notifier GC purges subscriptions for tasks done with no activity for this many days so stale rows aren't scanned forever. 0 = off.",
		"bot_mode":                              "Bot Mode cross-connection relay: envelopes queued by message_agent for agents on other connections wait in an on-disk outbox until the Desktop drains them.",
		"bot_mode.envelope_ttl_seconds":         "Drain-time TTL (seconds): older envelopes are NOT delivered on drain; the sender gets an error reply (reason 'queued_expired') so a DM can't land hours late as a zombie. 0 = no drain-time expiry (the 6h stale-artifact sweep still applies).",
		"bot_mode.turn_wait_seconds":            "How long a second delivery into a busy target profile queues behind the current turn before failing with a structured 'target_busy' error. Deliveries are serialized per profile with a cross-process file lock.",
		"code_execution":                        "execute_code settings (programmatic tool calls).",
		"code_execution.mode":                   "project = run in the session cwd with the active virtual environment or conda interpreter so project deps and relative paths resolve. strict = isolated temp dir with icebath's own interpreter: max isolation, project deps and relative paths won't work. Env scrubbing (*_API_KEY, *_TOKEN, *_SECRET, ...) and the tool whitelist apply in both modes.",
		"code_execution.kernel_idle_timeout":    "Session kernels are always on locally and remotely (a backend that cannot spawn a kernel fails open to per-call). One kernel per (session owner, mode, interpreter, cwd, tool-set) keeps state across calls and turns; subagents get their own. Kernels die with the session, after kernel_idle_timeout idle seconds, or by LRU eviction past max_session_kernels. A timed-out or interrupted cell kills the kernel; env is frozen at spawn (reset=true after changing passthrough). Tool RPC authority (approval, session, allow-list, call budget) is rebound per cell, and that runtime boundary is the cross-cell enforcement.",
		"code_execution.max_session_kernels":    "LRU cap on live session kernels.",
		"code_execution.timeout":                "Wall-clock limit (seconds) of one execute_code run (default 300).",
		"code_execution.max_tool_calls":         "Most tool calls one execute_code run may make through the tool bridge (default 50).",
		"code_execution.kernel_mode":            "Ignored; session kernels are always on.",
	}
}
