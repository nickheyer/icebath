package config

import (
	"reflect"

	"gopkg.in/yaml.v3"
)

// First-touch onboarding hint latches
type OnboardingConfig struct {
	Seen         Map[Raw]           `yaml:"seen"`
	ProfileBuild ProfileBuildPrompt `yaml:"profile_build"`
	Run          *bool              `yaml:"run"`
	rest         rest
}

type onboardingFields OnboardingConfig

// Decodes the mapping and keeps unknown keys
func (o *OnboardingConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*onboardingFields)(o), &o.rest)
}

// Writes the mapping with unknown keys after the named ones
func (o OnboardingConfig) MarshalYAML() (any, error) {
	return encodeMapping(onboardingFields(o), o.rest)
}

// First-message profile build offer
type ProfileBuildPrompt string

// Profile build offers
const (
	ProfileBuildAsk ProfileBuildPrompt = "ask"
	ProfileBuildOff ProfileBuildPrompt = "off"
)

// Accepted profile build offers
func ProfileBuildPrompts() []ProfileBuildPrompt {
	return []ProfileBuildPrompt{ProfileBuildAsk, ProfileBuildOff}
}

// Opt-in aggregate metrics section
type TelemetryConfig struct {
	SharedMetrics SharedMetrics `yaml:"shared_metrics"`
	rest          rest
}

type telemetryFields TelemetryConfig

// Decodes the mapping and keeps unknown keys
func (t *TelemetryConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*telemetryFields)(t), &t.rest)
}

// Writes the mapping with unknown keys after the named ones
func (t TelemetryConfig) MarshalYAML() (any, error) {
	return encodeMapping(telemetryFields(t), t.rest)
}

// Shared metrics collection and upload consent
type SharedMetrics struct {
	Enabled  bool   `yaml:"enabled" coerce:"exact"`
	Send     bool   `yaml:"send" coerce:"exact"`
	Endpoint string `yaml:"endpoint"`
	rest     rest
}

type sharedMetricsFields SharedMetrics

// Decodes the mapping and keeps unknown keys
func (s *SharedMetrics) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*sharedMetricsFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s SharedMetrics) MarshalYAML() (any, error) {
	return encodeMapping(sharedMetricsFields(s), s.rest)
}

// Doctor command section
type DoctorConfig struct {
	LiveProbeTimeout float64 `yaml:"live_probe_timeout"`
	rest             rest
}

type doctorFields DoctorConfig

// Decodes the mapping and keeps unknown keys
func (d *DoctorConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*doctorFields)(d), &d.rest)
}

// Writes the mapping with unknown keys after the named ones
func (d DoctorConfig) MarshalYAML() (any, error) {
	return encodeMapping(doctorFields(d), d.rest)
}

// Update behaviour section
type UpdatesConfig struct {
	Check                      bool                 `yaml:"check"`
	PreUpdateBackup            Scalar               `yaml:"pre_update_backup"`
	BackupKeep                 int                  `yaml:"backup_keep"`
	NonInteractiveLocalChanges LocalChangesPolicy   `yaml:"non_interactive_local_changes"`
	AutoSwitchParkedBranch     bool                 `yaml:"auto_switch_parked_branch" coerce:"bool"`
	ParkedBranchStrategy       ParkedBranchStrategy `yaml:"parked_branch_strategy"`
	RefreshCUADriver           bool                 `yaml:"refresh_cua_driver" coerce:"bool"`
	rest                       rest
}

type updatesFields UpdatesConfig

// Decodes the mapping and keeps unknown keys
func (u *UpdatesConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*updatesFields)(u), &u.rest)
}

// Writes the mapping with unknown keys after the named ones
func (u UpdatesConfig) MarshalYAML() (any, error) {
	return encodeMapping(updatesFields(u), u.rest)
}

// Pre-update backup modes, legacy true reading as full and false as off
func PreUpdateBackupModes() []string {
	return []string{"quick", "full", "off"}
}

// What non-interactive updates do with uncommitted source changes
type LocalChangesPolicy string

// Local change policies
const (
	LocalChangesStash   LocalChangesPolicy = "stash"
	LocalChangesDiscard LocalChangesPolicy = "discard"
)

// Accepted local change policies
func LocalChangesPolicies() []LocalChangesPolicy {
	return []LocalChangesPolicy{LocalChangesStash, LocalChangesDiscard}
}

// What an update does on a parked branch with unmerged commits
type ParkedBranchStrategy string

// Parked branch strategies
const (
	ParkedSwitch        ParkedBranchStrategy = "switch"
	ParkedUpdateInPlace ParkedBranchStrategy = "update_in_place"
)

// Accepted parked branch strategies
func ParkedBranchStrategies() []ParkedBranchStrategy {
	return []ParkedBranchStrategy{ParkedSwitch, ParkedUpdateInPlace}
}

// Language server diagnostics section
type LSPConfig struct {
	Enabled            bool            `yaml:"enabled"`
	WaitMode           LSPWaitMode     `yaml:"wait_mode"`
	WaitTimeout        float64         `yaml:"wait_timeout"`
	WarmupTimeout      float64         `yaml:"warmup_timeout"`
	BrokenRetrySeconds float64         `yaml:"broken_retry_seconds"`
	ExcludeRoots       PathList        `yaml:"exclude_roots"`
	TrustedWorkspaces  PathList        `yaml:"trusted_workspaces"`
	InstallStrategy    InstallStrategy `yaml:"install_strategy"`
	PackageManager     string          `yaml:"package_manager"`
	IdleTimeout        float64         `yaml:"idle_timeout"`
	Servers            Map[LSPServer]  `yaml:"servers"`
	rest               rest
}

type lspFields LSPConfig

// Decodes the mapping and keeps unknown keys
func (l *LSPConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*lspFields)(l), &l.rest)
}

// Writes the mapping with unknown keys after the named ones
func (l LSPConfig) MarshalYAML() (any, error) {
	return encodeMapping(lspFields(l), l.rest)
}

// List of path texts where null means none, a value of any other shape kept as written and reported invalid
type PathList struct {
	Paths []string
	held  *yaml.Node
}

// Builds a valid list of paths
func PathsOf(paths ...string) PathList {
	if paths == nil {
		paths = []string{}
	}
	return PathList{Paths: paths}
}

// Keeps an explicit null as a valid empty list written back as null
func (PathList) holdsNull() {}

// Accepts null or a list of text items, holding any other value as an invalid list
func (p *PathList) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if isNull(n) {
		*p = PathList{Paths: []string{}, held: nullNode()}
		return nil
	}
	if n.Kind == yaml.SequenceNode {
		paths := make([]string, 0, len(n.Content))
		for _, it := range n.Content {
			if it.Kind != yaml.ScalarNode || it.ShortTag() != tagStr {
				paths = nil
				break
			}
			paths = append(paths, it.Value)
		}
		if paths != nil {
			*p = PathList{Paths: paths}
			return nil
		}
	}
	*p = PathList{Paths: []string{}, held: plainCopy(n)}
	return nil
}

// Writes the held value while the list is unchanged since it was read, the paths otherwise
func (p PathList) MarshalYAML() (any, error) {
	if p.held != nil && len(p.Paths) == 0 {
		return emitted(p.held), nil
	}
	paths := p.Paths
	if paths == nil {
		paths = []string{}
	}
	return encodeValue(reflect.ValueOf(paths))
}

// Reports a value its readers take as a list of paths, false for any other shape or a non-text item
func (p PathList) Valid() bool {
	return p.held == nil || isNull(p.held)
}

// How long post-write lint waits for diagnostics
type LSPWaitMode string

// Diagnostic wait modes
const (
	LSPWaitDocument LSPWaitMode = "document"
	LSPWaitFull     LSPWaitMode = "full"
)

// Accepted diagnostic wait modes
func LSPWaitModes() []LSPWaitMode {
	return []LSPWaitMode{LSPWaitDocument, LSPWaitFull}
}

// How missing server binaries are obtained
type InstallStrategy string

// Install strategies
const (
	InstallAuto   InstallStrategy = "auto"
	InstallManual InstallStrategy = "manual"
	InstallOff    InstallStrategy = "off"
)

// Accepted install strategies, off reading as manual
func InstallStrategies() []InstallStrategy {
	return []InstallStrategy{InstallAuto, InstallManual, InstallOff}
}

// Node package managers for npm-recipe servers
func LSPPackageManagers() []string {
	return []string{"npm", "pnpm", "yarn"}
}

// Per-server override or custom server declaration
type LSPServer struct {
	Disabled              *bool       `yaml:"disabled,omitempty"`
	Command               []string    `yaml:"command,omitempty"`
	Env                   Map[string] `yaml:"env,omitempty"`
	InitializationOptions Map[Raw]    `yaml:"initialization_options,omitempty"`
	Extensions            []string    `yaml:"extensions,omitempty"`
	RootMarkers           []string    `yaml:"root_markers,omitempty"`
	LanguageID            string      `yaml:"language_id,omitempty"`
	Description           string      `yaml:"description,omitempty"`
	rest                  rest
}

type lspServerFields LSPServer

// Decodes the mapping and keeps unknown keys
func (l *LSPServer) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*lspServerFields)(l), &l.rest)
}

// Writes the mapping with unknown keys after the named ones
func (l LSPServer) MarshalYAML() (any, error) {
	return encodeMapping(lspServerFields(l), l.rest)
}

// X search tool section
type XSearchConfig struct {
	Model           string  `yaml:"model"`
	ReasoningEffort *string `yaml:"reasoning_effort"`
	TimeoutSeconds  int     `yaml:"timeout_seconds"`
	Retries         int     `yaml:"retries"`
	rest            rest
}

type xSearchFields XSearchConfig

// Decodes the mapping and keeps unknown keys
func (x *XSearchConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*xSearchFields)(x), &x.rest)
}

// Writes the mapping with unknown keys after the named ones
func (x XSearchConfig) MarshalYAML() (any, error) {
	return encodeMapping(xSearchFields(x), x.rest)
}

// Defaults of the updates section
func defaultUpdates() UpdatesConfig {
	return UpdatesConfig{
		Check:                      true,
		PreUpdateBackup:            Text("quick"),
		BackupKeep:                 5,
		NonInteractiveLocalChanges: LocalChangesStash,
		AutoSwitchParkedBranch:     true,
		ParkedBranchStrategy:       ParkedSwitch,
		RefreshCUADriver:           true,
	}
}

// Defaults of the LSP section
func defaultLSP() LSPConfig {
	return LSPConfig{
		Enabled:           true,
		WaitMode:          LSPWaitDocument,
		WaitTimeout:       5.0,
		ExcludeRoots:      PathsOf(),
		TrustedWorkspaces: PathsOf(),
		InstallStrategy:   InstallAuto,
		PackageManager:    "npm",
		IdleTimeout:       600.0,
	}
}

// Documentation of the onboarding, telemetry, doctor, updates, LSP and X search sections
func maintenanceDocs() map[string]string {
	return map[string]string{
		"onboarding":                            "First-touch onboarding hints, each latched under `seen` once shown.",
		"onboarding.profile_build":              "First-ever message: ask = offer; off = plain intro only.",
		"onboarding.run":                        "Desktop first run, root profile only: null = fresh install; true = next launch; false = done.",
		"telemetry":                             "Privacy-safe aggregate metrics in this profile's local telemetry dir. Collection (`enabled`) and upload (`send`) are separate keys that every consent surface sets together.",
		"telemetry.shared_metrics.send":         "Requires `enabled` (`send` alone logs an error). A package is sent only if its whole period is inside a recorded consent window.",
		"telemetry.shared_metrics.endpoint":     "Ingest endpoint (override for staging or local). Deliberately NOT env-overridable. Non-HTTPS refused unless the host is localhost.",
		"doctor.live_probe_timeout":             "Per-probe timeout (seconds) for `icebath doctor --live` real-call probes.",
		"updates.check":                         "Passive version and banner checks only; explicit `icebath update --check` remains enabled.",
		"updates.pre_update_backup":             "Pre-update backup. quick = snapshot small critical state (pairing JSONs, cron jobs, config.yaml, .env, auth.json, profile DBs) into <ICEBATH_HOME>/state-snapshots/, skipping files >1 GiB; restore via /snapshot. full = quick PLUS an `icebath backup` zip in <ICEBATH_HOME>/backups/ (`icebath import` restores; slow on large homes; `--backup` forces once). off = none (`--no-backup` forces once). Legacy booleans: true -> full, false -> off.",
		"updates.backup_keep":                   "Full backup zips to retain (older ones pruned after each success; floored to 1 so the newest is always kept). The quick snapshot always keeps exactly 1.",
		"updates.non_interactive_local_changes": "Uncommitted source-tree changes during NON-interactive updates (desktop, gateway; no TTY; interactive updates always stash and ask). stash = stash, pull, restore on top (conflicts stay in a git stash). discard = stash and drop after the pull (stash-and-drop, not reset --hard + clean -fd, so ignored paths are never touched).",
		"updates.auto_switch_parked_branch":     "If the checkout is parked on a feature branch and the tree is clean, switch to the update target (commits stay on the branch; a loud notice names it) so non-interactive updates keep working. A DIRTY tree blocks the switch and the code update is SKIPPED with a loud warning. false = never auto-switch.",
		"updates.parked_branch_strategy":        "Clean parked branch with unmerged commits: switch = move to the update target, commits stay on the branch (never conflicts). update_in_place = for a maintained custom branch, merge origin/<target> INTO it after leaving a pre-update-<stamp> tag; a conflict stops the update cleanly. `icebath update --switch-branch` overrides to switch for one run.",
		"updates.refresh_cua_driver":            "Refresh an installed cua-driver during `icebath update` (macOS only; a refresh failure never fails the update). Turn off e.g. on non-admin accounts where /Applications isn't writable.",
		"lsp":                                   "LSP diagnostics (pyright, gopls, rust-analyzer...) in the post-write lint check of write_file and patch. Runs only when the cwd or edited file is inside a git worktree; otherwise dormant and the in-process syntax check is the only tier.",
		"lsp.enabled":                           "false disables the whole subsystem: no servers, no event loop, no cost.",
		"lsp.wait_mode":                         "document = wait up to wait_timeout seconds for the current file's diagnostics; full = also request workspace-wide diagnostics (slower).",
		"lsp.warmup_timeout":                    "Budget for the FIRST request against a workspace whose server is not running yet (spawn + initialize + the server's initial program build; tsserver on a large project can need a minute). Once the client is up, wait_timeout applies again. 0 = same as wait_timeout.",
		"lsp.broken_retry_seconds":              "After a server fails (spawn error or outer timeout) its (server, workspace root) pair is skipped. 0 = for the process lifetime (until `icebath lsp restart`); N = retried after N seconds, so one transient stall does not silence a workspace forever.",
		"lsp.exclude_roots":                     "Workspace roots (glob patterns, ~ expanded; a bare path also matches everything under it) where no language server runs at all, e.g. one huge monorepo whose server cannot finish in budget, while every other workspace keeps its diagnostics. Must be a list: any other shape logs a warning and skips LSP for every workspace until fixed.",
		"lsp.trusted_workspaces":                "Directories (~ expanded; everything under an entry counts) whose projects a language server may load code from: the project's own virtual environment interpreter, node_modules TypeScript SDK, svelte.config.js, build files (cargo, Gradle, mix, ...). The worktree of the launch dir or the session's workspace (icebath -w, a Desktop project, terminal.cwd) is always trusted; in any other checkout (a clone the agent made) only servers that run no project code start, pinned to icebath-side tools, and the npx tsc and rustfmt lint fallbacks are skipped.",
		"lsp.install_strategy":                  "Missing server binaries: auto = install via npm, go or pip into <ICEBATH_HOME>/lsp/bin/ on first use; manual = only binaries on PATH; off = alias for manual.",
		"lsp.package_manager":                   "Node package manager for the npm-recipe servers: npm | pnpm | yarn. Installs still land in <ICEBATH_HOME>/lsp/node_modules; a configured manager that is not installed, or an unknown value, skips the install (no silent fallback to npm) so a pnpm or yarn supply-chain policy is never bypassed.",
		"lsp.idle_timeout":                      "Idle seconds before a server is shut down (respawned on demand), so long-running processes don't accumulate stale children (hundreds of MB + pipe FDs each) across worktrees. 0 = keep servers for the process lifetime.",
		"lsp.servers":                           "Per-server overrides keyed by registry server_id (pyright, gopls...): disabled: true; command: [\"path/to/server\", \"--stdio\"] (bypasses auto-install); env: {...}; initialization_options: {...} (merged into LSP initializationOptions). A key that is NOT a built-in id declares a custom server (matched before the built-ins): command: [\"my-ls\", \"--stdio\"]; extensions: [\".ext\"]; optional root_markers: [...], language_id: \"...\" (didOpen languageId), description: \"...\". Manual install only.",
		"x_search":                              "X (Twitter) Search via xAI's x_search Responses tool. Registers when xAI creds exist (SuperGrok OAuth or XAI_API_KEY) AND the toolset is enabled in `icebath tools`.",
		"x_search.model":                        "xAI model for the Responses call; any Grok model with x_search access works.",
		"x_search.reasoning_effort":             "Reasoning effort for models that support it: low, medium, high or xhigh; null keeps the model default.",
		"x_search.timeout_seconds":              "Request timeout in seconds (minimum 30); complex queries can take 60-120s.",
		"x_search.retries":                      "Retries on 5xx, read timeouts and connection errors (backoff 1.5x attempt s, cap 5s).",
	}
}

// Reasoning efforts the X search tool sends, read case-insensitively with null or empty keeping the model default
func XSearchReasoningEfforts() []string {
	return []string{"low", "medium", "high", "xhigh"}
}
