package config

import "gopkg.in/yaml.v3"

// Context engine section
type ContextConfig struct {
	Engine     string           `yaml:"engine"`
	MemoryTrim MemoryTrimConfig `yaml:"memory_trim"`
	rest       rest
}

type contextFields ContextConfig

// Decodes the mapping and keeps unknown keys
func (c *ContextConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*contextFields)(c), &c.rest)
}

// Writes the mapping with unknown keys after the named ones
func (c ContextConfig) MarshalYAML() (any, error) {
	return encodeMapping(contextFields(c), c.rest)
}

// Return of freed allocator pages at cleanup boundaries
type MemoryTrimConfig struct {
	Enabled           bool    `yaml:"enabled" coerce:"strict"`
	CooldownSeconds   float64 `yaml:"cooldown_seconds"`
	LogEveryN         int     `yaml:"log_every_n"`
	InfoLogMinDeltaMB float64 `yaml:"info_log_min_delta_mb"`
	rest              rest
}

type memoryTrimFields MemoryTrimConfig

// Decodes the mapping and keeps unknown keys
func (m *MemoryTrimConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*memoryTrimFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m MemoryTrimConfig) MarshalYAML() (any, error) {
	return encodeMapping(memoryTrimFields(m), m.rest)
}

// Persistent curated memory section
type MemoryConfig struct {
	MemoryEnabled        bool   `yaml:"memory_enabled"`
	UserProfileEnabled   bool   `yaml:"user_profile_enabled"`
	PrefetchSpillEnabled bool   `yaml:"prefetch_spill_enabled" coerce:"strict"`
	WriteApproval        bool   `yaml:"write_approval" coerce:"approval"`
	MemoryCharLimit      int    `yaml:"memory_char_limit"`
	UserCharLimit        int    `yaml:"user_char_limit"`
	NudgeInterval        int    `yaml:"nudge_interval"`
	Provider             string `yaml:"provider"`
	rest                 rest
}

type memoryFields MemoryConfig

// Decodes the mapping and keeps provider settings and unknown keys
func (m *MemoryConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*memoryFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m MemoryConfig) MarshalYAML() (any, error) {
	return encodeMapping(memoryFields(m), m.rest)
}

// Memory provider names, bundled first then catalog-installed
func MemoryProviders() []string {
	return []string{"", "retaindb", "byterover", "honcho", "hindsight", "supermemory", "mem0", "openviking", "holographic"}
}

// Subagent delegation section, with the max_async_children key of older configs
type DelegationConfig struct {
	Backend                          string          `yaml:"backend"`
	Backends                         Map[Raw]        `yaml:"backends"`
	Model                            string          `yaml:"model"`
	Provider                         string          `yaml:"provider"`
	FallbackProviders                *FallbackChain  `yaml:"fallback_providers"`
	BaseURL                          string          `yaml:"base_url"`
	APIKey                           string          `yaml:"api_key"`
	APIMode                          string          `yaml:"api_mode"`
	RequestOverrides                 Map[Raw]        `yaml:"request_overrides"`
	CompressionThresholdTokens       int             `yaml:"compression_threshold_tokens" coerce:"exact"`
	InheritMCPToolsets               bool            `yaml:"inherit_mcp_toolsets"`
	MaxIterations                    int             `yaml:"max_iterations"`
	MaxSummaryChars                  int             `yaml:"max_summary_chars"`
	ChildTimeoutSeconds              float64         `yaml:"child_timeout_seconds"`
	ReasoningEffort                  ReasoningEffort `yaml:"reasoning_effort"`
	MaxConcurrentChildren            int             `yaml:"max_concurrent_children"`
	IndependentCompletions           bool            `yaml:"independent_completions"`
	GatewayMode                      DelegationMode  `yaml:"gateway_mode"`
	MaxSpawnDepth                    int             `yaml:"max_spawn_depth"`
	OrchestratorEnabled              bool            `yaml:"orchestrator_enabled"`
	OneshotMaxChildren               int             `yaml:"oneshot_max_children"`
	SubagentAutoApprove              bool            `yaml:"subagent_auto_approve"`
	SurfaceChildProcessNotifications bool            `yaml:"surface_child_process_notifications" coerce:"bool"`
	MaxAsyncChildren                 *int            `yaml:"max_async_children,omitempty"`
	WorktreeIsolation                bool            `yaml:"worktree_isolation,omitempty" coerce:"bool"`
	rest                             rest
}

type delegationFields DelegationConfig

// Decodes the mapping and keeps unknown keys
func (d *DelegationConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*delegationFields)(d), &d.rest)
}

// Writes the mapping with unknown keys after the named ones
func (d DelegationConfig) MarshalYAML() (any, error) {
	return encodeMapping(delegationFields(d), d.rest)
}

// How a gateway turn waits for delegated work
type DelegationMode string

// Delegation gateway modes
const (
	DelegateAuto       DelegationMode = "auto"
	DelegateInline     DelegationMode = "inline"
	DelegateBackground DelegationMode = "background"
	DelegateAsync      DelegationMode = "async"
)

// Accepted delegation gateway modes, async an alias of background and any other value auto
func DelegationModes() []DelegationMode {
	return []DelegationMode{DelegateAuto, DelegateInline, DelegateBackground, DelegateAsync}
}

// Persistent goal loop section
type GoalsConfig struct {
	MaxTurns int `yaml:"max_turns"`
	rest     rest
}

type goalsFields GoalsConfig

// Decodes the mapping and keeps unknown keys
func (g *GoalsConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*goalsFields)(g), &g.rest)
}

// Writes the mapping with unknown keys after the named ones
func (g GoalsConfig) MarshalYAML() (any, error) {
	return encodeMapping(goalsFields(g), g.rest)
}

// In-session loop cadence bounds
type LoopsConfig struct {
	MinIntervalSeconds      int `yaml:"min_interval_seconds"`
	MaxTicks                int `yaml:"max_ticks"`
	SelfPacedFloorSeconds   int `yaml:"self_paced_floor_seconds"`
	SelfPacedCeilingSeconds int `yaml:"self_paced_ceiling_seconds"`
	rest                    rest
}

type loopsFields LoopsConfig

// Decodes the mapping and keeps unknown keys
func (l *LoopsConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*loopsFields)(l), &l.rest)
}

// Writes the mapping with unknown keys after the named ones
func (l LoopsConfig) MarshalYAML() (any, error) {
	return encodeMapping(loopsFields(l), l.rest)
}

// Defaults of the delegation section
func defaultDelegation() DelegationConfig {
	return DelegationConfig{
		Backend:               "default",
		InheritMCPToolsets:    true,
		MaxIterations:         250,
		MaxSummaryChars:       24000,
		ReasoningEffort:       EffortLevel(""),
		MaxConcurrentChildren: 10,
		GatewayMode:           DelegateAuto,
		MaxSpawnDepth:         1,
		OrchestratorEnabled:   true,
		OneshotMaxChildren:    2,
	}
}

// Documentation of the context, memory, delegation, goal and loop sections
func memoryDocs() map[string]string {
	return map[string]string{
		"context":                                   "Context engine: how the context window is managed near the token limit. \"compressor\" = built-in lossy summarization; or a plugin name (e.g. \"lcm\") installed in plugins/context_engine/<name>/ or ~/.icebath/plugins/.",
		"context.memory_trim":                       "Return freed allocator pages at agent and TUI cleanup boundaries (no-op elsewhere).",
		"context.memory_trim.log_every_n":           "INFO-log every Nth periodic trim; force paths always log.",
		"context.memory_trim.info_log_min_delta_mb": "Suppress INFO logs when the readable RSS delta is smaller; 0 = log all.",
		"memory":                                         "Persistent memory: bounded curated memory injected into the system prompt.",
		"memory.prefetch_spill_enabled":                  "External recall opt-in to hooks.output_spill.",
		"memory.write_approval":                          "Approval gate for memory writes on BOTH foreground turns and the background review fork. true = foreground writes prompt inline; background writes are staged (/memory pending|approve <id>|reject <id>). To disable memory use memory_enabled.",
		"memory.memory_char_limit":                       "About 800 tokens at 2.75 chars per token.",
		"memory.user_char_limit":                         "About 500 tokens at 2.75 chars per token.",
		"memory.nudge_interval":                          "Periodic built-in memory review; 0 when an external provider auto-extracts.",
		"memory.provider":                                "One external provider: bundled (retaindb, byterover) or catalog-installed (honcho, hindsight, supermemory, mem0, openviking, holographic). Empty = built-in only.",
		"delegation":                                     "Subagent delegation: `backend` = task-routing strategy (\"default\": the keys below), `backends.<name>` = its settings.",
		"delegation.model":                               "e.g. \"google/gemini-3-flash-preview\" (empty = inherit parent).",
		"delegation.provider":                            "e.g. \"openrouter\" (empty = inherit the parent provider + credentials).",
		"delegation.fallback_providers":                  "Fallback chain for delegated children (same entry format as the top-level list). For an unpinned child, null = inherit the parent chain; [] = disable fallback. A child pinned by provider, endpoint, or model gets no fallback unless this setting declares one explicitly.",
		"delegation.base_url":                            "Direct OpenAI-compatible endpoint for subagents.",
		"delegation.api_key":                             "Key for delegation.base_url (falls back to OPENAI_API_KEY).",
		"delegation.api_mode":                            "Wire protocol for delegation.base_url: \"chat_completions\" | \"codex_responses\" | \"anthropic_messages\". Empty = auto-detect from the URL (e.g. /anthropic suffix); set explicitly for non-standard endpoints.",
		"delegation.request_overrides":                   "Per-child request settings on every delegation call (all resolution branches). Top-level keys = API kwargs (e.g. service_tier); an \"extra_body\" sub-dict merges into extra_body, e.g. {\"extra_body\": {\"provider\": {\"sort\": \"throughput\"}}}. Explicit values win OVER runtime and parent overrides (extra_body deep-merged 1 level).",
		"delegation.compression_threshold_tokens":        "Optional absolute cap on a subagent's compaction TRIGGER (not the request payload), applied as the lower of this and the child's ratio threshold. 0 (default) = no subagent-specific cap; children compact where the parent does, the lower of 0.50 x window and the global compression.threshold_tokens cap. Every compaction is a chance to lose detail, so the default stays off. A token count >= 16000 enables it; other values (true, \"200k\") are config errors, warned and ignored.",
		"delegation.inherit_mcp_toolsets":                "When delegate_task narrows child toolsets, keep the parent's enabled MCP toolsets (so toolsets=[\"web\"] doesn't strip MCP). false = strict intersection.",
		"delegation.max_iterations":                      "Per-subagent iteration cap (own budget, independent of the parent's).",
		"delegation.max_summary_chars":                   "Hard per-summary char ceiling on subagent results, layered on the dynamic budget (each summary is sized to the parent's remaining context headroom; trimmed text spills to ~/.icebath/cache/delegation/ with a head+tail window + read_file offset footer, nothing lost). 0 disables the ceiling; the dynamic budget still applies.",
		"delegation.child_timeout_seconds":               "Inactivity cap per child (seconds, floor 30): time with NO progress, not total runtime. 0 = no cap: children fail only from real errors (API, tools, iteration budget). A progressing child (including one waiting on a multi-minute completion) restarts the window; a frozen one is caught.",
		"delegation.reasoning_effort":                    "Subagent effort: \"ultra\" | \"max\" | \"xhigh\" | \"high\" | \"medium\" | \"low\" | \"minimal\" | \"none\" (empty = inherit).",
		"delegation.max_concurrent_children":             "Max parallel children per batch AND max concurrent background delegation units; async dispatches beyond it run synchronously. Floor 1, no ceiling.",
		"delegation.independent_completions":             "Background fan-outs return as ONE message when the whole call finishes. true = each task (or `group`) returns on its own as it finishes, more new turns for the orchestrator.",
		"delegation.gateway_mode":                        "Gateway: inline (join in the turn) | background | auto = inline where interim messages are off.",
		"delegation.max_spawn_depth":                     "Orchestrator role depth, floored at 1, no ceiling; each level multiplies cost. 1 = flat, 2 = orchestrator -> leaf, 3+ = deeper.",
		"delegation.orchestrator_enabled":                "Kill switch for role=\"orchestrator\".",
		"delegation.oneshot_max_children":                "Total subagents a finite one-shot run (icebath chat -q / --oneshot) may spawn; 0 = unlimited. Each child re-pays a cold system prompt and re-explores the repo, and one-shot spawns are mostly \"review my own work\" rather than parallel work.",
		"delegation.subagent_auto_approve":               "Subagent threads ALWAYS resolve approvals non-interactively (the parent TUI owns stdin). false = auto-deny, true = auto-approve \"once\"; both log a warning audit line. true only for trusted batch work.",
		"delegation.surface_child_process_notifications": "Subagent background processes (task_id \"sa-...\") route notify_on_complete and watch_pattern notifications to the PARENT; false suppresses them (the child's result is the deliverable). Async-delegation results are NEVER suppressed.",
		"delegation.max_async_children":                  "Deprecated; use delegation.max_concurrent_children.",
		"delegation.worktree_isolation":                  "Each child gets its own git worktree off the parent's HEAD so parallel children never contend for one working copy. Git-only and local-backend-only; otherwise ignored.",
		"prefill_messages_file":                          "Ephemeral prefill messages file: a JSON list of {role, content} dicts injected at the start of every API call for few-shot priming. Never saved to sessions, logs or trajectories.",
		"goals":                                          "Goals: persistent cross-turn /goal loop. After each turn an aux-model judge checks if the goal is satisfied, else a continuation prompt re-enters the session until done, budget exhausted, or paused. Judge failures fail OPEN; the budget is the backstop.",
		"goals.max_turns":                                "Max continuation turns before auto-pause (/goal resume), guarding against judge false negatives and unbounded spend.",
		"loops":                                          "Loops: /loop re-runs a prompt or slash command on a cadence in-session. A fixed interval fires on the user's clock; self-paced (no interval) starts at the floor and backs off exponentially while replies stop changing.",
		"loops.min_interval_seconds":                     "Smallest fixed interval; tighter cadences are raised to it.",
		"loops.max_ticks":                                "Auto-pause after this many wakeups unless --times is set; 0 = unlimited.",
		"loops.self_paced_floor_seconds":                 "Self-paced cadence lower bound (seconds).",
		"loops.self_paced_ceiling_seconds":               "Self-paced cadence upper bound (seconds).",
	}
}
