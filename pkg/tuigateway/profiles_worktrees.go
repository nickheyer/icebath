package tuigateway

type ProfilesListParams struct {
	Profile         *string `json:"profile,omitzero"`
	IncludeSessions *bool   `json:"include_sessions,omitzero"`
}

// Newest session of a profile, with the live compression tip and root title when it is the profile's bot chat
type ProfileSession struct {
	ID               string  `json:"id"`
	ResolvedID       string  `json:"resolved_id,omitzero"`
	RootTitle        string  `json:"root_title,omitzero"`
	Title            string  `json:"title,omitzero"`
	Preview          string  `json:"preview,omitzero"`
	StartedAt        float64 `json:"started_at,omitzero"`
	LastActive       float64 `json:"last_active,omitzero"`
	MessageCount     int64   `json:"message_count,omitzero"`
	LiveMessageCount *int64  `json:"live_message_count,omitzero"`
}

// Newest kanban or tool worker session of a profile, so a roster shows it as working
type ProfileWorkerSession struct {
	ID         string  `json:"id"`
	Source     string  `json:"source,omitzero"`
	Title      string  `json:"title,omitzero"`
	LastActive float64 `json:"last_active,omitzero"`
}

// One roster row, its session fields present only when sessions were asked for
type ProfileRow struct {
	Name             string                `json:"name"`
	Path             string                `json:"path"`
	IsDefault        bool                  `json:"is_default,omitzero"`
	Model            *string               `json:"model,omitzero"`
	Provider         *string               `json:"provider,omitzero"`
	Description      string                `json:"description,omitzero"`
	DisplayName      string                `json:"display_name,omitzero"`
	SkillCount       int64                 `json:"skill_count,omitzero"`
	PreviousNames    []string              `json:"previous_names,omitzero"`
	LastSession      *ProfileSession       `json:"last_session,omitzero"`
	WorkerSession    *ProfileWorkerSession `json:"worker_session,omitzero"`
	CanonicalSession *ProfileSession       `json:"canonical_session,omitzero"`
	UIMetaRevisions  map[string]int64      `json:"ui_meta_revisions,omitzero"`
	UIMeta           map[string]any        `json:"ui_meta,omitzero"`
	HasAvatar        bool                  `json:"has_avatar,omitzero"`
}

// Profile roster with previews, the backend injecting the teammate protocol itself and the machine that answered
type ProfilesListResult struct {
	Profiles        []ProfileRow `json:"profiles,omitzero"`
	BotModeProtocol *bool        `json:"bot_mode_protocol,omitzero"`
	InstallID       string       `json:"install_id,omitzero"`
}

// Profile to export, the connection's own when empty, and the absolute archive path, the export directory when empty
type ProfilesExportParams struct {
	Name   string `json:"name,omitzero"`
	Output string `json:"output,omitzero"`
}

type ProfilesExportResult struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Absolute path of a shared archive and the profile name that overrides the one it carries
type ProfilesImportParams struct {
	Archive string `json:"archive"`
	Name    string `json:"name,omitzero"`
}

// Imported profile and its alias script, null when the name collides with a command or the script could not be written
type ProfilesImportResult struct {
	Name    string  `json:"name"`
	Path    string  `json:"path"`
	Wrapper *string `json:"wrapper"`
}

type SnapshotListParams struct{}

// Words after the /snapshot subcommand
type SnapshotArgsParams struct {
	Args []string `json:"args,omitzero"`
}

// Lines in the profile's language, shown as they come
type SnapshotLines struct {
	Lines []string `json:"lines"`
}

// Snapshot a restore names by id or list number, null with lines when none is named or the number is off
type SnapshotTargetResult struct {
	ID    *string  `json:"id"`
	Lines []string `json:"lines"`
}

// Git worktree with its branch and the repository it belongs to
type WorktreeRef struct {
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	RepoRoot string `json:"repo_root"`
}

type WorktreeLaunch struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

// Folder to look up and the launch folder and process whose -w launch locked a tree
type WorktreeStatusParams struct {
	Cwd       string `json:"cwd"`
	LaunchCwd string `json:"launch_cwd"`
	PID       int64  `json:"pid"`
}

type WorktreeStatusResult struct {
	RepoRoot *string         `json:"repo_root"`
	Launch   *WorktreeLaunch `json:"launch"`
}

type WorktreeCwdParams struct {
	Cwd string `json:"cwd"`
}

// Repository of the folder and the git worktree list, empty when git failed
type WorktreeListResult struct {
	RepoRoot *string  `json:"repo_root"`
	Lines    []string `json:"lines"`
}

// Folder whose repository is pruned, a dry run, and the tree in use that is never reclaimed
type WorktreePruneParams struct {
	Cwd    string `json:"cwd"`
	DryRun bool   `json:"dry_run,omitzero"`
	Keep   string `json:"keep,omitzero"`
}

type WorktreeKept struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

type WorktreePruneResult struct {
	RepoRoot  *string        `json:"repo_root"`
	Actions   []string       `json:"actions"`
	Preserved []WorktreeKept `json:"preserved"`
	Error     *string        `json:"error"`
}

// Folder whose repository gets the tree, its name, and the process the tree is locked for
type WorktreeCreateParams struct {
	Cwd  string `json:"cwd"`
	Name string `json:"name,omitzero"`
	PID  int64  `json:"pid"`
}

// Creation report and the tree, null when the creation failed
type WorktreeCreateResult struct {
	RepoRoot *string      `json:"repo_root"`
	Tree     *WorktreeRef `json:"tree"`
	Lines    []string     `json:"lines"`
}

type WorktreeRecordResult struct {
	Ok bool `json:"ok"`
}

// Lines saying the tree was removed or why it is kept
type WorktreeSettleResult struct {
	Lines []string `json:"lines"`
}

// Profile roster with previews so a client paints without a call per profile
var ProfilesList = NewMethod[ProfilesListParams, ProfilesListResult]("profiles.list")

// Profile roster of the session authority, sessions included unless turned off
var CanonicalProfilesList = NewCanonicalMethod[ProfilesListParams, ProfilesListResult]("profiles.list")

// Writes a profile, its config, skills and theme, to a shareable archive
var ProfilesExport = NewMethod[ProfilesExportParams, ProfilesExportResult]("profiles.export")

// Imports a shared profile archive as a new profile with its wrapper alias
var ProfilesImport = NewMethod[ProfilesImportParams, ProfilesImportResult]("profiles.import")

// Quick state snapshots of the profile as a numbered table
var SnapshotList = NewMethod[SnapshotListParams, SnapshotLines]("snapshot.list")

// Takes a quick state snapshot, the args joined as its label
var SnapshotCreate = NewMethod[SnapshotArgsParams, SnapshotLines]("snapshot.create")

// Keeps the newest N quick snapshots, twenty when no count is given
var SnapshotPrune = NewMethod[SnapshotArgsParams, SnapshotLines]("snapshot.prune")

// Resolves the snapshot a /snapshot restore names
var SnapshotTarget = NewMethod[SnapshotArgsParams, SnapshotTargetResult]("snapshot.target")

// Repository of a folder and the tree a -w launch locked
var WorktreeStatus = NewMethod[WorktreeStatusParams, WorktreeStatusResult]("worktree.status")

// Git worktrees of the folder's repository
var WorktreeList = NewMethod[WorktreeCwdParams, WorktreeListResult]("worktree.list")

// Reclaims stale trees and merged branches, or names what a dry run would reclaim
var WorktreePrune = NewMethod[WorktreePruneParams, WorktreePruneResult]("worktree.prune")

// Creates an isolated git worktree on a new branch
var WorktreeCreate = NewMethod[WorktreeCreateParams, WorktreeCreateResult]("worktree.create")

// Remembers a tree a session moved into, so its resume finds it
var WorktreeRecord = NewMethod[WorktreeRef, WorktreeRecordResult]("worktree.record")

// Removes a clean tree and its branch, or says why it is kept
var WorktreeSettle = NewMethod[WorktreeRef, WorktreeSettleResult]("worktree.settle")
