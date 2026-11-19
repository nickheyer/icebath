package tuigateway

// Session whose working directory checkpoints are listed
type RollbackListParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
}

// One checkpoint with the reason it was taken as its message
type RollbackCheckpoint struct {
	Hash      string `json:"hash"`
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
}

// Checkpoints newest first, enabled false when checkpointing is off
type RollbackListResult struct {
	Enabled     bool                 `json:"enabled"`
	Checkpoints []RollbackCheckpoint `json:"checkpoints"`
}

// Checkpoint by hash or one based number, one file or the whole tree, all also taking files the user edited
type RollbackRestoreParams struct {
	SessionID string  `json:"session_id"`
	Hash      string  `json:"hash"`
	FilePath  string  `json:"file_path,omitzero"`
	Profile   *string `json:"profile,omitzero"`
	All       *bool   `json:"all,omitzero"`
}

// Restore outcome, the files restored and skipped, and the turns a whole tree restore rewound
type RollbackRestoreResult struct {
	Success          bool     `json:"success"`
	RestoredTo       string   `json:"restored_to,omitzero"`
	Reason           string   `json:"reason,omitzero"`
	Directory        string   `json:"directory,omitzero"`
	File             string   `json:"file,omitzero"`
	RestoredFiles    []string `json:"restored_files,omitzero"`
	SkippedUserEdits []string `json:"skipped_user_edits,omitzero"`
	SkippedOversize  []string `json:"skipped_oversize,omitzero"`
	FailedDeletes    []string `json:"failed_deletes,omitzero"`
	HistoryRemoved   *int64   `json:"history_removed,omitzero"`
	Error            string   `json:"error,omitzero"`
	Debug            string   `json:"debug,omitzero"`
}

// Checkpoint a diff compares the working tree with
type RollbackDiffParams struct {
	SessionID string  `json:"session_id"`
	Hash      string  `json:"hash"`
	Profile   *string `json:"profile,omitzero"`
}

// Diff stat and body cut to four thousand characters, with its rendering sized to the terminal when one renders
type RollbackDiffResult struct {
	Stat     string `json:"stat"`
	Diff     string `json:"diff"`
	Rendered string `json:"rendered,omitzero"`
}

// Session whose whole change since its earliest retained checkpoint is read
type RollbackSessionDiffParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
}

// Session diff as data, the body cut to one frame by whole lines while diff_lines counts all of it
type RollbackSessionDiffResult struct {
	Enabled   bool   `json:"enabled"`
	Success   bool   `json:"success,omitzero"`
	Stat      string `json:"stat,omitzero"`
	Diff      string `json:"diff,omitzero"`
	DiffLines int64  `json:"diff_lines,omitzero"`
	Empty     bool   `json:"empty,omitzero"`
	Error     string `json:"error,omitzero"`
}

// Checkpoints of the session's working directory
var RollbackList = NewMethod[RollbackListParams, RollbackListResult]("rollback.list")

// Restores the working tree or one file to a checkpoint
var RollbackRestore = NewMethod[RollbackRestoreParams, RollbackRestoreResult]("rollback.restore")

// Diff between a checkpoint and the working tree
var RollbackDiff = NewMethod[RollbackDiffParams, RollbackDiffResult]("rollback.diff")

// Everything the session changed since its earliest retained checkpoint
var RollbackSessionDiff = NewCanonicalMethod[RollbackSessionDiffParams, RollbackSessionDiffResult]("rollback.session_diff")
