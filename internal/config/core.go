package config

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// SQLite journal and sizing section
type DatabaseConfig struct {
	JournalMode       JournalMode `yaml:"journal_mode"`
	WALAutocheckpoint *int        `yaml:"wal_autocheckpoint" coerce:"nofloat"`
	JournalSizeLimit  *int64      `yaml:"journal_size_limit" coerce:"nofloat"`
	CacheSize         *int64      `yaml:"cache_size,omitempty" coerce:"nofloat"`
	MmapSize          *int64      `yaml:"mmap_size,omitempty" coerce:"nofloat"`
	TempStore         *int        `yaml:"temp_store,omitempty" coerce:"nofloat"`
	Synchronous       *Scalar     `yaml:"synchronous,omitempty"`
	rest              rest
}

type databaseFields DatabaseConfig

// Decodes the mapping and keeps unknown keys
func (d *DatabaseConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*databaseFields)(d), &d.rest)
}

// Writes the mapping with unknown keys after the named ones
func (d DatabaseConfig) MarshalYAML() (any, error) {
	return encodeMapping(databaseFields(d), d.rest)
}

// SQLite journal mode of every database
type JournalMode string

// Journal modes
const (
	JournalWAL    JournalMode = "wal"
	JournalDelete JournalMode = "delete"
)

// Accepted journal modes, any other value reading as wal
func JournalModes() []JournalMode {
	return []JournalMode{JournalWAL, JournalDelete}
}

// Durability level names, also accepted as 0 to 3 and false for OFF
func SynchronousLevels() []string {
	return []string{"OFF", "NORMAL", "FULL", "EXTRA"}
}

// Server process resource limit section
type RuntimeConfig struct {
	NofileSoftLimit *Scalar `yaml:"nofile_soft_limit"`
	MallocArenaMax  *Scalar `yaml:"malloc_arena_max"`
	rest            rest
}

type runtimeFields RuntimeConfig

// Decodes the mapping and keeps unknown keys
func (r *RuntimeConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*runtimeFields)(r), &r.rest)
}

// Writes the mapping with unknown keys after the named ones
func (r RuntimeConfig) MarshalYAML() (any, error) {
	return encodeMapping(runtimeFields(r), r.rest)
}

// Positive limit a runtime key asks for, ok false when null, false, zero, negative or not an integer disables it
func RuntimeLimit(v *Scalar) (limit int, ok bool) {
	if v == nil || v.Kind() != KindInt {
		return 0, false
	}
	n, isInt := v.AsInt()
	if !isInt || n <= 0 {
		return 0, false
	}
	return n, true
}

// Per-terminal session resume section
type SessionConfig struct {
	TerminalContinue bool `yaml:"terminal_continue" coerce:"bool"`
	rest             rest
}

type sessionFields SessionConfig

// Decodes the mapping and keeps unknown keys
func (s *SessionConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*sessionFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s SessionConfig) MarshalYAML() (any, error) {
	return encodeMapping(sessionFields(s), s.rest)
}

// Attachment staging section
type AttachmentsConfig struct {
	Storage AttachmentStorage `yaml:"storage"`
	rest    rest
}

type attachmentsFields AttachmentsConfig

// Decodes the mapping and keeps unknown keys
func (a *AttachmentsConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*attachmentsFields)(a), &a.rest)
}

// Writes the mapping with unknown keys after the named ones
func (a AttachmentsConfig) MarshalYAML() (any, error) {
	return encodeMapping(attachmentsFields(a), a.rest)
}

// Where session attachments are staged
type AttachmentStorage string

// Attachment stores
const (
	AttachIcebathHome AttachmentStorage = "icebath-home"
	AttachWorkspace   AttachmentStorage = "workspace"
)

// Accepted attachment stores, any other value reading as the profile home
func AttachmentStorages() []AttachmentStorage {
	return []AttachmentStorage{AttachIcebathHome, AttachWorkspace}
}

// OpenRouter provider routing preferences, flat values with per-model overrides
type ProviderRouting struct {
	Sort              string               `yaml:"sort,omitempty"`
	Only              []string             `yaml:"only,omitempty"`
	Ignore            []string             `yaml:"ignore,omitempty"`
	Order             []string             `yaml:"order,omitempty"`
	RequireParameters *bool                `yaml:"require_parameters,omitempty"`
	DataCollection    string               `yaml:"data_collection,omitempty"`
	Models            Map[ProviderRouting] `yaml:"models,omitempty"`
	rest              rest
}

type providerRoutingFields ProviderRouting

// Decodes the mapping and keeps unknown keys
func (p *ProviderRouting) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*providerRoutingFields)(p), &p.rest)
}

// Writes the mapping with unknown keys after the named ones
func (p ProviderRouting) MarshalYAML() (any, error) {
	return encodeMapping(providerRoutingFields(p), p.rest)
}

// OpenRouter provider sort strategies
func ProviderSorts() []string {
	return []string{"price", "throughput", "latency"}
}

// Operation deadline overrides as a tree addressed by dotted keys such as tools.concurrent_batch
type TimeoutTree struct {
	Map[Raw]
}

// Value at a dotted key, ok false when any segment is missing or not a mapping
func (t TimeoutTree) Lookup(key string) (Scalar, bool) {
	parts := strings.Split(key, ".")
	raw, ok := t.Get(parts[0])
	if !ok || raw.node == nil {
		return Scalar{}, false
	}
	cur := raw.node
	for _, part := range parts[1:] {
		var next *yaml.Node
		for _, p := range mappingPairs(cur) {
			if p.key.Value == part {
				next = resolveAlias(p.value)
			}
		}
		if next == nil {
			return Scalar{}, false
		}
		cur = next
	}
	cur = resolveAlias(cur)
	if cur.Kind != yaml.ScalarNode {
		return Scalar{}, false
	}
	return Scalar{tag: cur.ShortTag(), value: cur.Value}, true
}

// Setup wizard routing switch
type SmartModelRouting struct {
	Enabled *bool `yaml:"enabled,omitempty"`
	rest    rest
}

type smartModelRoutingFields SmartModelRouting

// Decodes the mapping and keeps unknown keys
func (s *SmartModelRouting) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*smartModelRoutingFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s SmartModelRouting) MarshalYAML() (any, error) {
	return encodeMapping(smartModelRoutingFields(s), s.rest)
}

// Documentation of the small core sections
func coreDocs() map[string]string {
	return map[string]string{
		"database":                          "SQLite settings for every icebath database.",
		"database.journal_mode":             "SQLite journal mode for every icebath DB. \"wal\" default; use \"delete\" on weak-fsync or shared filesystems where WAL is not crash-safe (macOS virtiofs, NFS, SMB).",
		"database.wal_autocheckpoint":       "WAL sizing pragma (pages between automatic checkpoints). null = SQLite default (1000 pages).",
		"database.journal_size_limit":       "WAL sizing pragma (bytes cap on the WAL or journal file). null = SQLite default (no limit).",
		"database.synchronous":              "Durability level for every state.db connection: OFF, NORMAL, FULL, EXTRA (or 0-3). Unset leaves SQLite's compile-time default, which differs between builds. On macOS this is a floor: values below FULL are refused because Darwin's fsync() does not guarantee write ordering, while EXTRA is honored normally.",
		"runtime":                           "Server-process fd soft limit (clamped to the hard limit) + glibc arena cap; 0, false or null = off.",
		"runtime.nofile_soft_limit":         "Long-running icebath server processes raise their RLIMIT_NOFILE soft limit to this value when the OS permits it, clamped to the hard limit and never lowering a higher soft limit. 0, false, or null disables the adjustment.",
		"runtime.malloc_arena_max":          "Caps glibc malloc arenas in the gateway and `icebath serve` process (Linux glibc only; child commands keep glibc's default). An explicit MALLOC_ARENA_MAX (exported or in .env) wins. 0, false, or null = glibc default (8 per CPU core).",
		"max_concurrent_sessions":           "Global active chat session cap across CLI, TUI and dashboard, and messaging. null or 0 = unbounded.",
		"max_live_sessions":                 "Soft LRU cap on in-memory TUI, desktop and dashboard sessions. Above it the gateway evicts the least-recently-active DETACHED sessions (no live client); reopening re-resumes from disk. 0 or null disables.",
		"session.terminal_continue":         "Per-terminal `icebath -c`: each CLI session writes a breadcrumb under $ICEBATH_HOME/terminal-sessions/<terminal-id>, so bare -c/--continue resumes THIS terminal's session (tmux, kitty or wezterm pane, tty). false = resume the globally most-recent.",
		"attachments.storage":               "Where the TUI and desktop gateway stages session file attachments (uploads, pasted text). \"icebath-home\" (default) keeps <profile home>/attachments, the dir container backends bind-mount, so @file: refs resolve in the sandbox. \"workspace\" opts into <session workspace>/.icebath/attachments: staging lands inside the allowed ref root, so the same profile's agent can always read its own attachments back. Read per profile from that profile's config; a remote (ssh) workspace keeps the profile home dir either way.",
		"provider_routing":                  "OpenRouter provider routing (only applies when using OpenRouter): sort (price, throughput, latency), only, ignore, order, require_parameters, data_collection (allow or deny), and per-model overrides under models.",
		"timeouts":                          "Operation deadline overrides (seconds) by dotted key, e.g. tools.concurrent_batch and tools.sequential_call. Precedence: this section > legacy ICEBATH_* env var > built-in default. 0 or a negative value disables the bound; very large values are clamped to a platform-safe maximum.",
		"worktree":                          "Always create an isolated git worktree per CLI session in a git repo (equivalent to always passing --worktree / -w).",
		"worktree_sync":                     "Branch a new worktree from the freshly-fetched remote tip (default true); false branches from local HEAD (offline or pinned base).",
		"paste_collapse_threshold":          "Paste collapse thresholds (TUI + CLI); 0 disables each. Bracketed pastes with this many newlines collapse to a file reference.",
		"paste_collapse_threshold_fallback": "Same test for terminals without bracketed paste, gated by chars and newlines-added heuristics.",
		"paste_collapse_char_threshold":     "Single-line pastes this long collapse too.",
		"_config_version":                   "Config schema version, bumped when new required fields are added.",
	}
}
