package config

import "gopkg.in/yaml.v3"

// Stale git worktree lifecycle settings
type WorktreesConfig struct {
	ArchiveDirty              bool    `yaml:"archive_dirty" coerce:"boolkeep"`
	ArchiveMaxFileMB          float64 `yaml:"archive_max_file_mb"`
	ArchiveRetentionDays      float64 `yaml:"archive_retention_days"`
	MaxTotalGB                float64 `yaml:"max_total_gb"`
	InstallStateRetentionDays float64 `yaml:"install_state_retention_days" coerce:"exact"`
	rest                      rest
}

type worktreesFields WorktreesConfig

// Decodes the mapping and keeps unknown keys
func (w *WorktreesConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*worktreesFields)(w), &w.rest)
}

// Writes the mapping with unknown keys after the named ones
func (w WorktreesConfig) MarshalYAML() (any, error) {
	return encodeMapping(worktreesFields(w), w.rest)
}

// Filesystem checkpoint settings
type CheckpointsConfig struct {
	Enabled          bool `yaml:"enabled" coerce:"bool"`
	MaxSnapshots     int  `yaml:"max_snapshots"`
	MaxTotalSizeMB   int  `yaml:"max_total_size_mb"`
	MaxFileSizeMB    int  `yaml:"max_file_size_mb"`
	AutoPrune        bool `yaml:"auto_prune" coerce:"bool"`
	RetentionDays    int  `yaml:"retention_days"`
	MinIntervalHours int  `yaml:"min_interval_hours"`
	bare             bareForm
	rest             rest
}

type checkpointsFields CheckpointsConfig

// Decodes the mapping and keeps unknown keys, a bare boolean standing for enabled and any other scalar for defaults
func (c *CheckpointsConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeSwitchable(n, (*checkpointsFields)(c), &c.rest, &c.bare, func(s *yaml.Node) {
		if s.ShortTag() == tagBool {
			c.Enabled = s.Value == "true"
		}
	})
}

// Writes the bare scalar the file held while nothing changed, else the mapping with unknown keys last
func (c CheckpointsConfig) MarshalYAML() (any, error) {
	return encodeSwitchable(checkpointsFields(c), c.rest, c.bare)
}

// MCP runtime behavior, distinct from the mcp_servers definitions
type MCPConfig struct {
	AutoReloadOnConfigChange bool `yaml:"auto_reload_on_config_change" coerce:"bool"`
	DiscoveryConcurrency     int  `yaml:"discovery_concurrency" coerce:"exactint"`
	rest                     rest
}

type mcpFields MCPConfig

// Decodes the mapping and keeps unknown keys
func (m *MCPConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*mcpFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m MCPConfig) MarshalYAML() (any, error) {
	return encodeMapping(mcpFields(m), m.rest)
}

// Tool output truncation limits
type ToolOutputConfig struct {
	MaxBytes      int `yaml:"max_bytes"`
	MaxLines      int `yaml:"max_lines"`
	MaxLineLength int `yaml:"max_line_length"`
	rest          rest
}

type toolOutputFields ToolOutputConfig

// Decodes the mapping and keeps unknown keys
func (t *ToolOutputConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*toolOutputFields)(t), &t.rest)
}

// Writes the mapping with unknown keys after the named ones
func (t ToolOutputConfig) MarshalYAML() (any, error) {
	return encodeMapping(toolOutputFields(t), t.rest)
}

// Tool loop guardrails, with the flat legacy threshold keys load reads when the nested ones are absent
type ToolLoopGuardrails struct {
	WarningsEnabled               bool            `yaml:"warnings_enabled"`
	HardStopEnabled               bool            `yaml:"hard_stop_enabled"`
	NonInteractiveHardStopEnabled bool            `yaml:"non_interactive_hard_stop_enabled"`
	WarnAfter                     LoopThresholds  `yaml:"warn_after"`
	HardStopAfter                 LoopThresholds  `yaml:"hard_stop_after"`
	LoopCaps                      LoopCaps        `yaml:"loop_caps"`
	Legacy                        legacyThreshold `yaml:",inline"`
	rest                          rest
}

// Flat threshold keys of older configs, each naming one nested threshold
type legacyThreshold struct {
	ExactFailureWarnAfter    *int `yaml:"exact_failure_warn_after,omitempty"`
	SameToolFailureWarnAfter *int `yaml:"same_tool_failure_warn_after,omitempty"`
	NoProgressWarnAfter      *int `yaml:"no_progress_warn_after,omitempty"`
	ExactFailureBlockAfter   *int `yaml:"exact_failure_block_after,omitempty"`
	SameToolFailureHaltAfter *int `yaml:"same_tool_failure_halt_after,omitempty"`
	NoProgressBlockAfter     *int `yaml:"no_progress_block_after,omitempty"`
}

type toolLoopGuardrailsFields ToolLoopGuardrails

// Decodes the mapping and keeps unknown keys
func (t *ToolLoopGuardrails) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*toolLoopGuardrailsFields)(t), &t.rest)
}

// Writes the mapping with unknown keys after the named ones
func (t ToolLoopGuardrails) MarshalYAML() (any, error) {
	return encodeMapping(toolLoopGuardrailsFields(t), t.rest)
}

type ToolBudgetConfig struct {
	MCPResultSizeChars int `yaml:"mcp_result_size_chars"`
	rest               rest
}

type toolBudgetFields ToolBudgetConfig

func (t *ToolBudgetConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*toolBudgetFields)(t), &t.rest)
}

func (t ToolBudgetConfig) MarshalYAML() (any, error) {
	return encodeMapping(toolBudgetFields(t), t.rest)
}

// Repeat counts that trigger a guardrail action
type LoopThresholds struct {
	ExactFailure         int `yaml:"exact_failure"`
	SameToolFailure      int `yaml:"same_tool_failure"`
	IdempotentNoProgress int `yaml:"idempotent_no_progress"`
	rest                 rest
}

type loopThresholdsFields LoopThresholds

// Decodes the mapping and keeps unknown keys
func (l *LoopThresholds) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*loopThresholdsFields)(l), &l.rest)
}

// Writes the mapping with unknown keys after the named ones
func (l LoopThresholds) MarshalYAML() (any, error) {
	return encodeMapping(loopThresholdsFields(l), l.rest)
}

// Per-turn hard ceilings for runaway-prone tools
type LoopCaps struct {
	MaxWebSearches int `yaml:"max_web_searches"`
	MaxSubagents   int `yaml:"max_subagents"`
	rest           rest
}

type loopCapsFields LoopCaps

// Decodes the mapping and keeps unknown keys
func (l *LoopCaps) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*loopCapsFields)(l), &l.rest)
}

// Writes the mapping with unknown keys after the named ones
func (l LoopCaps) MarshalYAML() (any, error) {
	return encodeMapping(loopCapsFields(l), l.rest)
}

// Defaults of the worktree, checkpoint, MCP and tool output sections
func defaultWorktrees() WorktreesConfig {
	return WorktreesConfig{ArchiveDirty: true, ArchiveMaxFileMB: 50, ArchiveRetentionDays: 30, MaxTotalGB: 0, InstallStateRetentionDays: 14}
}

// Defaults of the checkpoint section
func defaultCheckpoints() CheckpointsConfig {
	return CheckpointsConfig{MaxSnapshots: 20, MaxTotalSizeMB: 500, MaxFileSizeMB: 10, AutoPrune: true, RetentionDays: 7, MinIntervalHours: 24}
}

// Defaults of the tool loop guardrails
func defaultGuardrails() ToolLoopGuardrails {
	return ToolLoopGuardrails{
		WarningsEnabled:               true,
		NonInteractiveHardStopEnabled: true,
		WarnAfter:                     LoopThresholds{ExactFailure: 2, SameToolFailure: 3, IdempotentNoProgress: 2},
		HardStopAfter:                 LoopThresholds{ExactFailure: 5, SameToolFailure: 8, IdempotentNoProgress: 5},
		LoopCaps:                      LoopCaps{MaxWebSearches: 50, MaxSubagents: 50},
	}
}

// Documentation of the workspace and tool limit keys
func workspaceDocs() map[string]string {
	return map[string]string{
		"worktrees":                                              "Stale git worktree lifecycle (`icebath -w`, `/worktree new`, subagent isolation, Desktop). The sweep runs at `icebath -w` startup and every 6h from the cron ticker over every repo icebath made a worktree in. Trees idle past their tier (icebath-*/subagent-* 24h, named 72h) that are clean and merged are removed; dirty or unpushed ones are archived to refs/icebath/archive/<tree> first (`icebath worktree restore <name>`). Live trees and Kanban t_<hex> trees are never touched.",
		"worktrees.archive_dirty":                                "false = never remove a dirty or unpushed tree (the pre-archive behaviour).",
		"worktrees.archive_max_file_mb":                          "Untracked files larger than this (MB) are not archived, so a tree holding one is kept.",
		"worktrees.archive_retention_days":                       "The sweep deletes archive refs older than this many days. 0 keeps them forever.",
		"worktrees.max_total_gb":                                 "Optional size budget (GB) for idle trees: past it the oldest idle trees are archived and removed first. Live, young and Kanban trees are never counted or touched. 0 = off.",
		"worktrees.install_state_retention_days":                 "installs/<key> of a missing checkout, unused this long, goes; 0 = never.",
		"checkpoints":                                            "Filesystem checkpoints: snapshot the working directory once per turn (on the first write_file or patch call); restore with /rollback. Opt-in via `icebath chat --checkpoints` or enabled=true (most users never use /rollback). Single shared shadow store with real pruning.",
		"checkpoints.enabled":                                    "Take a checkpoint on the first write of every turn.",
		"checkpoints.max_snapshots":                              "Max checkpoints per working directory; enforced by ref rewrite + GC of older commits.",
		"checkpoints.max_total_size_mb":                          "Hard ceiling on total ~/.icebath/checkpoints/ size (MB); the oldest checkpoint per project is dropped round-robin until under the cap. 0 disables.",
		"checkpoints.max_file_size_mb":                           "Skip files larger than this (MB) when staging (datasets, model weights). 0 = no filter.",
		"checkpoints.auto_prune":                                 "Background sweep (CLI helper thread or gateway housekeeping tick, at most once per min_interval_hours; never on the startup path, since its git gc can block for tens of seconds): deletes projects whose last_touch is older than retention_days, GCs the shared store when refs moved, enforces max_total_size_mb, deletes legacy-* archives older than retention_days. It NEVER deletes orphans (workdir missing on disk): a missing workdir may just be an unmounted volume or VPN, and an unattended sweep must not guess. Orphans: `icebath checkpoints prune` (`--keep-orphans` to skip).",
		"checkpoints.retention_days":                             "Days of inactivity before a project's checkpoints are pruned.",
		"checkpoints.min_interval_hours":                         "Minimum hours between background prune sweeps.",
		"context_file_max_chars":                                 "Hard cap (chars) for one auto-loaded context file (SOUL.md, AGENTS.md, CLAUDE.md, .icebath.md, .cursorrules) before head/tail truncation. null = scale with the model's context window (floor 20K, ceiling 500K); a positive int pins a fixed cap. Separate from read_file limits.",
		"context_file_read_timeout":                              "Seconds to wait for a single context file read before skipping it with a warning. Guards startup against network-backed filesystems (iCloud Drive, OneDrive, NFS) that can block a cold read.",
		"file_read_max_chars":                                    "Max chars per read_file call; larger reads are rejected with offset+limit guidance. 100K chars is about 25 to 35K tokens.",
		"mcp_discovery_timeout":                                  "Seconds the first agent build waits for background MCP discovery before snapshotting its tool list. Returns the instant discovery completes (no MCP servers -> ~0s); the bound only bites when a server is still connecting. Turn-1 latency knob only: a server that misses it is picked up by the between-turns refresh, so keep it small, since a dead server adds this much to first-response latency.",
		"mcp_single_query_discovery_timeout":                     "Same bound for single-query mode (`icebath -q/-z`). With only ONE turn there is no between-turns refresh, so a server that misses the window is invisible for the whole session; the larger bound lets slow cold-start servers (npx, uvx, remote HTTP) land. Reachable servers still only wait their real handshake time.",
		"mcp":                                                    "MCP runtime behavior (distinct from mcp_servers definitions and auxiliary.mcp).",
		"mcp.auto_reload_on_config_change":                       "Auto-reload MCP connections when config.yaml's mcp_servers changes (CLI watcher). Every reload rebuilds the tool surface and INVALIDATES the provider prompt cache (the next message re-sends the full prefix), costly on long-context models. When false the watcher still detects the change and prints /reload-mcp guidance.",
		"mcp.discovery_concurrency":                              "Max MCP servers connected (and their stdio child trees spawned) at once per discovery pass, at boot, on /reload-mcp and on the config watcher's reconcile. Unbounded, a config with N servers spawns N process trees in the same instant (RAM/CPU spike, 429 fan-out on multi-profile fleets). 0 = unlimited.",
		"tool_output":                                            "Tool-output truncation.",
		"tool_output.max_bytes":                                  "terminal_tool output cap in chars (head+tail kept; 50000 is about 12 to 15K tokens).",
		"tool_output.max_lines":                                  "Max `limit` one read_file call may request before clamping.",
		"tool_output.max_line_length":                            "Per-line cap in read_file's line-numbered view (chars).",
		"tool_loop_guardrails":                                   "Tool loop guardrails nudge models that repeat failed or non-progressing tool calls. Soft warnings are always on; hard stops are opt-in so interactive sessions keep flowing.",
		"tool_loop_guardrails.warnings_enabled":                  "Append a warning when a tool call repeats without progress.",
		"tool_loop_guardrails.hard_stop_enabled":                 "Stop the turn once a hard_stop_after threshold is reached on interactive surfaces.",
		"tool_loop_guardrails.non_interactive_hard_stop_enabled": "Unattended gateway and cron platforms hard-stop by default (nobody can /stop a model that ignores warnings); interactive cli, tui, desktop and acp stay warning-only.",
		"tool_loop_guardrails.warn_after":                        "Repeat counts that add a warning: exact_failure, same_tool_failure, idempotent_no_progress.",
		"tool_loop_guardrails.hard_stop_after":                   "Repeat counts that stop the turn when hard stops are enabled.",
		"tool_loop_guardrails.loop_caps":                         "Per-turn hard ceilings for runaway-prone tools; counters reset every turn, always on regardless of the thresholds above. Dozens of searches or subagents in ONE turn is already pathological, hence low defaults. 0 = unlimited.",
		"tool_loop_guardrails.loop_caps.max_web_searches":        "web_search calls per turn.",
		"tool_loop_guardrails.loop_caps.max_subagents":           "Subagents spawned per turn.",
		"tool_budget":                                            "Tool result persistence budget: results over a threshold are saved under cache/spillover and replaced by a preview and the file path.",
		"tool_budget.mcp_result_size_chars":                      "Per-result threshold (chars) for mcp_ tools, capped at the general per-result threshold; MCP servers often return un-paginated payloads. 0 or a negative value uses the default 50000.",
	}
}
