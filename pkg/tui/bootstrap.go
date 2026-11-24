package tui

import (
	"context"
	"time"
)

// Private WebSocket target of the home's gateway owner the terminal UI attaches with
type Grant struct {
	URL        string
	Protocols  []string
	ProfileID  string
	InstanceID string
	// Build of an idle gateway the attach replaced by one of this build, empty when none was
	Replaced string
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
	// Stops the home's background service of another build so the next attach starts one of this build, nil where the launcher cannot
	ReplaceOwner func(ctx context.Context) error
	// Settings of the window the launcher resolved for the home, the built-in defaults when nil
	Prefs *Prefs
}

// Settings of the window itself, resolved by the launcher before the background service answers since the terminal UI reads no settings of its own
type Prefs struct {
	// Theme the profile sets at launch: auto, light or dark; light and dark pin the mode before the background service answers
	Theme string
	// Window animations turned off
	ReducedMotion bool
	// Rows one wheel step scrolls before it speeds up
	ScrollSpeed float64
	// Termux layout defaults apply when the window runs inside Termux
	TermuxLayout bool
	// Longest wait for the background service to answer a request
	RequestTimeout time.Duration
	// Longest wait for the background service to be ready
	StartupTimeout time.Duration
	// Destructive commands run without their confirmation
	NoConfirm bool
	// Copies always (true) or never (false) go through the terminal's clipboard sequence, nil deciding by the connection
	ClipboardSequence *bool
	// File the CPU profile goes to, empty for none
	CPUProfile string
	// Frame rate readout over the window
	FrameRate bool
	// Timing log of the slow frames
	FrameLog bool
	// Frames drawn faster than this stay out of the timing log
	FrameLogFloor time.Duration
	// File of the timing log, empty for perf.log in the icebath folder
	FrameLogFile string
	// Folder the memory dumps go to, empty for heapdumps in the icebath folder
	HeapDumpDir string
	// Memory watchdog writes whole heap profiles and not the facts alone
	AutoHeapProfile bool
	// Bytes the memory dump folder may hold
	HeapDumpMaxBytes int64
	// Shortest wait between two automatic memory dumps
	HeapDumpCooldown time.Duration
	// Memory dump written as soon as the window starts
	HeapDumpOnStart bool
}

// Settings of the window in effect, the built-in defaults until Run takes the ones the launcher resolved
var prefs = Prefs{Theme: "auto", ScrollSpeed: 1, TermuxLayout: true, RequestTimeout: 2 * time.Minute, StartupTimeout: 15 * time.Second,
	FrameLogFloor: 2 * time.Millisecond, HeapDumpMaxBytes: heapDumpCap, HeapDumpCooldown: memoryDumpCooldown}
