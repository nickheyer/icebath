package config

const (
	Key   = "sessions"
	Store = Key + ".store"
	// Deletes old sessions when the server starts, and how old they must be
	AutoPrune     = Key + ".auto_prune"
	RetentionDays = Key + ".retention_days"
	// Least time between automatic archive or cleanup runs
	MinIntervalHours = Key + ".min_interval_hours"
	VacuumAfterPrune = Key + ".vacuum_after_prune"
	// Least time between compactions of the store
	MinVacuumIntervalDays = Key + ".min_vacuum_interval_days"
	AutoArchive           = Key + ".auto_archive"
	AutoArchiveDays       = Key + ".auto_archive_days"
	// Closes, when the server starts, the sessions a process that died left open
	StartupOrphanSweep = Key + ".startup_orphan_sweep"
	// How long a session no window watches waits before it is closed
	OrphanEndGrace = Key + ".orphan_end_grace_s"
	// How long a session is kept after its last window went away, and how long it may sit idle so
	OrphanReapGrace     = Key + ".ws_orphan_reap_grace_s"
	OrphanActivityStale = Key + ".ws_orphan_activity_stale_s"
	MaxExportMessages   = Key + ".max_export_messages"
	MaxResumeMessages   = Key + ".max_resume_messages"
	ShowSubagents       = Key + ".show_subagents"
	// Sessions one window, every window of one profile and the server keep open, and those running a turn at once
	MaxLivePerConnection = Key + ".max_live_per_connection"
	MaxLivePerProfile    = Key + ".max_live_per_profile"
	MaxLive              = Key + ".max_live_sessions"
	MaxConcurrent        = Key + ".max_concurrent_sessions"
	// Remembers each terminal's session so it can be continued there
	TerminalContinue = Key + ".terminal_continue"
)

// Turns cut short when the server stopped, run again when it starts
const (
	AutoContinue          = Key + ".auto_continue"
	AutoContinueEnabled   = AutoContinue + ".enabled"
	AutoContinueFreshness = AutoContinue + ".freshness_minutes"
	AutoContinueAttempts  = AutoContinue + ".max_attempts"
)
