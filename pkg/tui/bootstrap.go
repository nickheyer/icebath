package tui

import "context"

// Private WebSocket target of the home's gateway owner the terminal UI attaches with
type Grant struct {
	URL        string
	Protocols  []string
	ProfileID  string
	InstanceID string
}

// Outcome of a quick snapshot restore, the lines to show and whether the socket was released for it
type SnapshotRestore struct {
	Lines       []string
	Released    bool
	SessionKept bool
}

// Tree a /worktree new created, as the gateway described it
type Worktree struct {
	Path     string
	Branch   string
	RepoRoot string
}

// Work the launcher does for the terminal UI on the home's gateway owner and its files, outside the wire
type Host struct {
	Attach          func(ctx context.Context, start, recover bool) (Grant, error)
	RestoreSnapshot func(ctx context.Context, id, session string, handoff bool, release func()) SnapshotRestore
	SettleWorktrees func(ctx context.Context, trees []Worktree, active string)
}
