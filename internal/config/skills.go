package config

import "gopkg.in/yaml.v3"

// Skills section, with the disable lists and skill-declared settings unset unless written
type SkillsConfig struct {
	ExternalDirs       StringList      `yaml:"external_dirs"`
	CreateDir          string          `yaml:"create_dir" coerce:"exact"`
	ProjectDiscovery   bool            `yaml:"project_discovery" coerce:"strict"`
	TrustedProjectDirs StringList      `yaml:"trusted_project_dirs"`
	AutoLoad           []string        `yaml:"auto_load" coerce:"exact"`
	TemplateVars       bool            `yaml:"template_vars" coerce:"bool"`
	InlineShell        bool            `yaml:"inline_shell" coerce:"bool"`
	InlineShellTimeout int             `yaml:"inline_shell_timeout"`
	GuardAgentCreated  bool            `yaml:"guard_agent_created"`
	Tier1Advisory      bool            `yaml:"tier1_advisory" coerce:"boolwords"`
	WriteApproval      bool            `yaml:"write_approval" coerce:"approval"`
	Ledger             bool            `yaml:"ledger" coerce:"bool"`
	LedgerMaxBytes     int64           `yaml:"ledger_max_bytes"`
	Disabled           *StringList     `yaml:"disabled,omitempty"`
	PlatformDisabled   Map[StringList] `yaml:"platform_disabled,omitempty"`
	Config             Map[Raw]        `yaml:"config,omitempty"`
	rest               rest
}

type skillsFields SkillsConfig

// Decodes the mapping and keeps unknown keys
func (s *SkillsConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*skillsFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s SkillsConfig) MarshalYAML() (any, error) {
	return encodeMapping(skillsFields(s), s.rest)
}

// Background skill maintenance section
type CuratorConfig struct {
	Enabled          bool          `yaml:"enabled" coerce:"bool"`
	IntervalHours    int           `yaml:"interval_hours"`
	MinIdleHours     float64       `yaml:"min_idle_hours"`
	StaleAfterDays   int           `yaml:"stale_after_days"`
	ArchiveAfterDays int           `yaml:"archive_after_days"`
	Consolidate      bool          `yaml:"consolidate" coerce:"bool"`
	PruneBuiltins    bool          `yaml:"prune_builtins" coerce:"bool"`
	ArchiveTTLDays   int           `yaml:"archive_ttl_days"`
	Backup           CuratorBackup `yaml:"backup"`
	Provider         string        `yaml:"provider,omitempty"`
	Model            string        `yaml:"model,omitempty"`
	rest             rest
}

type curatorFields CuratorConfig

// Decodes the mapping and keeps unknown keys
func (c *CuratorConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*curatorFields)(c), &c.rest)
}

// Writes the mapping with unknown keys after the named ones
func (c CuratorConfig) MarshalYAML() (any, error) {
	return encodeMapping(curatorFields(c), c.rest)
}

// Snapshot taken before a consolidation pass
type CuratorBackup struct {
	Enabled bool `yaml:"enabled" coerce:"bool"`
	Keep    int  `yaml:"keep"`
	rest    rest
}

type curatorBackupFields CuratorBackup

// Decodes the mapping and keeps unknown keys
func (c *CuratorBackup) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*curatorBackupFields)(c), &c.rest)
}

// Writes the mapping with unknown keys after the named ones
func (c CuratorBackup) MarshalYAML() (any, error) {
	return encodeMapping(curatorBackupFields(c), c.rest)
}

// Defaults of the skills section
func defaultSkills() SkillsConfig {
	return SkillsConfig{
		ExternalDirs:       ListOf(),
		ProjectDiscovery:   true,
		TrustedProjectDirs: ListOf(),
		AutoLoad:           []string{},
		TemplateVars:       true,
		InlineShellTimeout: 10,
		Tier1Advisory:      true,
		Ledger:             true,
		LedgerMaxBytes:     5 * 1024 * 1024,
	}
}

// Defaults of the curator section
func defaultCurator() CuratorConfig {
	return CuratorConfig{
		Enabled:          true,
		IntervalHours:    24 * 7,
		MinIdleHours:     2,
		StaleAfterDays:   14,
		ArchiveAfterDays: 30,
		Backup:           CuratorBackup{Enabled: true, Keep: 2},
	}
}

// Documentation of the skills, curator, honcho and timezone keys
func skillsDocs() map[string]string {
	return map[string]string{
		"skills":                      "Skills: external skill directories shared across tools and agents. Paths are expanded (~, ${VAR}) and resolved; read-only, so creation goes to ~/.icebath/skills/ unless create_dir redirects it.",
		"skills.external_dirs":        "e.g. [\"~/.agents/skills\", \"/shared/team-skills\"].",
		"skills.create_dir":           "Where skill_manage-created skills go (empty = profile-local dir). When set, new skills land here AND agent-facing instructions name this path; expanded (~, ${VAR}), relative to ICEBATH_HOME, scanned alongside the local dir.",
		"skills.project_discovery":    "In a git checkout, <root>/.icebath/skills/ and <root>/.agents/skills/ load as the highest-precedence tier, ONLY if the root is in trusted_project_dirs. false = no scan, no untrusted-skills notice.",
		"skills.trusted_project_dirs": "Trusted project roots; managed by `icebath skills trust` and `untrust`.",
		"skills.auto_load":            "Skill names pinned as fully loaded in every new session (CLI, TUI, gateway, cron, API). Resolved once when the agent's prompt is first built; missing or disabled names warn and skip; ICEBATH_IGNORE_RULES suppresses the list like the other auto-injected context.",
		"skills.template_vars":        "Substitute ${ICEBATH_SKILL_DIR} and ${ICEBATH_SESSION_ID} in SKILL.md content.",
		"skills.inline_shell":         "Pre-execute !`cmd` snippets in SKILL.md, inlining stdout (dates, git state...). Off: host-unapproved skill-author code; community hub installs never auto-execute.",
		"skills.inline_shell_timeout": "Seconds per !`cmd` snippet.",
		"skills.guard_agent_created":  "Security-scan skills the agent writes via skill_manage. Off: the agent can run the same code via terminal() ungated, so it mostly blocks prose with risky keywords. On: a dangerous verdict is a tool error the agent can retry. Hub installs are always scanned.",
		"skills.tier1_advisory":       "Advisory NVIDIA SkillEvaluator Tier 1 scan on `icebath skills install` (alongside the enforcing built-in guard), only if `skillevaluator` is on PATH (uv tool install \"skillevaluator @ git+https://github.com/NVIDIA/SkillEvaluator.git\"). Informational, never blocking; secrets-class findings shown red. No-op without it.",
		"skills.write_approval":       "Approval gate for skill_manage mutations on BOTH foreground turns and the background review fork. true = ALWAYS stage (SKILL.md too large for an inline prompt): /skills pending, /skills diff <id>, /skills approve|reject <id>.",
		"skills.ledger":               "Audit ledger: every skill mutation appends to ~/.icebath/skills/.curator_ledger.jsonl with before and after hashes (blobs under ~/.icebath/.curator_backups/blobs/); powers `icebath curator ledger` and `rollback <entry-id>`. Never a gate: failures can't block.",
		"skills.ledger_max_bytes":     "Size cap for that ledger: once the file grows past this, the next append rewrites it through the unchanged-file dedup and, if still over, drops the oldest entries (0 = keep the ledger append-only forever, the previous behaviour).",
		"skills.disabled":             "Skill names disabled everywhere.",
		"skills.platform_disabled":    "Skill names disabled per platform.",
		"skills.config":               "Skill-declared settings, read by the skills that declare them.",
		"curator":                     "Curator: background maintenance of AGENT-CREATED skills (never hub-installed): marks long-unused skills stale, archives (never deletes) obsolete ones, optionally consolidates overlaps via a forked aux-model agent. Inactivity-triggered from session start, no cron daemon. `icebath curator status` shows the last run.",
		"curator.interval_hours":      "Hours between runs.",
		"curator.min_idle_hours":      "Only run after the agent has been idle this long.",
		"curator.stale_after_days":    "Mark \"stale\" after this many unused days.",
		"curator.archive_after_days":  "Move to skills/.archive/ (recoverable) after this many unused days.",
		"curator.consolidate":         "LLM consolidation (umbrella-building) pass. OFF = deterministic inactivity prune only, no aux-model cost. `icebath curator run --consolidate` overrides once.",
		"curator.prune_builtins":      "Also prune bundled built-ins (a suppression list stops `icebath update` restoring them); hub-installed skills are NEVER pruned. OFF by default: shipped skills vanishing from `skills_list` because nobody loaded them for 30 days surprised people. true = built-ins age out like agent-created skills.",
		"curator.archive_ttl_days":    "TTL purge of skills/.archive/: 0 = never; > 0 lets the explicit `icebath curator purge` delete older archived skills (never automatic; logged in the ledger).",
		"curator.backup":              "Before a consolidation pass (the only one that rewrites skill content in place), snapshot ~/.icebath/skills/ to ~/.icebath/skills/.curator_backups/<utc-iso>/skills.tar.gz (`icebath curator rollback`). The prune-only pass just moves directories into .archive/ and takes none.",
		"curator.backup.keep":         "Retain the last N regular snapshots.",
		"honcho":                      "Honcho memory plugin: ~/.honcho/config.json is the source of truth (apiKey, workspace, peerName, sessions, enabled); the plugin reads icebath-specific overrides from here.",
		"timezone":                    "IANA timezone (e.g. \"Asia/Kolkata\", \"America/New_York\"). Empty = server-local time.",
	}
}
