package settings

import "time"

// How much you are told when the agent saves to Memory
var DisplayMemoryNotifications = Register(&Enum{Meta: Meta{Key: "display.memory_notifications", Label: "Memory notices", Short: "How much you are told when the agent saves to Memory.", Group: GroupMemory, Order: 1000, Advanced: true}, Default: "on", Values: []Choice{
	{Value: "on", Label: "On", Meaning: "Brief note when Memory changes."},
	{Value: "verbose", Label: "Verbose", Meaning: "Note with what was saved."},
	{Value: "off", Label: "Off", Meaning: "No notes."},
}})

// Most characters the agent's own notes in Memory may hold
var MemoryMemoryCharLimit = Register(&Int{Meta: Meta{Key: "memory.memory_char_limit", Label: "Memory size limit", Short: "Most characters the agent's own notes in Memory may hold.", Group: GroupMemory, Order: 1010, Advanced: true, Apply: NewSession}, Default: 2200, Unit: "characters"})

// Turns between reminders to the agent to save to Memory
var MemoryNudgeInterval = Register(&Int{Meta: Meta{Key: "memory.nudge_interval", Label: "Memory reminder spacing", Short: "Turns between reminders to the agent to save to Memory.", Group: GroupMemory, Order: 1020, Advanced: true, Apply: NewSession}, Default: 10, Unit: "turns"})

// Move long recalled Memory to a file instead of the prompt
var MemoryPrefetchSpillEnabled = Register(&Bool{Meta: Meta{Key: "memory.prefetch_spill_enabled", Label: "Spill long recalls", Short: "Move long recalled Memory to a file instead of the prompt.", Help: "Applies to outside Memory providers only.", Group: GroupMemory, Order: 1030, Advanced: true}, Default: false})

// Outside service used for Memory
var MemoryProvider = Register(&String{Meta: Meta{Key: "memory.provider", Label: "Memory provider", Short: "Outside service used for Memory.", Help: "Empty uses only the built-in Memory.", Group: GroupMemory, Order: 1040, Advanced: true}, Default: ""})

// Most characters the notes about you in Memory may hold
var MemoryUserCharLimit = Register(&Int{Meta: Meta{Key: "memory.user_char_limit", Label: "Profile size limit", Short: "Most characters the notes about you in Memory may hold.", Group: GroupMemory, Order: 1050, Advanced: true, Apply: NewSession}, Default: 1375, Unit: "characters"})

// Remember each terminal's session so it can be continued there
var SessionTerminalContinue = Register(&Bool{Meta: Meta{Key: "session.terminal_continue", Label: "Continue per terminal", Short: "Remember each terminal's session so it can be continued there.", Group: GroupMemory, Order: 1060, Advanced: true}, Default: true})

// Archive sessions that have sat idle for a while
var SessionsAutoArchive = Register(&Bool{Meta: Meta{Key: "sessions.auto_archive", Label: "Auto-archive sessions", Short: "Archive sessions that have sat idle for a while.", Group: GroupMemory, Order: 1070, Advanced: true}, Default: false})

// How long a session must be idle before it is archived
var SessionsAutoArchiveDays = Register(&Duration{Meta: Meta{Key: "sessions.auto_archive_days", Label: "Archive after idle", Short: "How long a session must be idle before it is archived.", Help: "Pinned sessions are never archived.", Group: GroupMemory, Order: 1080, Advanced: true}, Default: 3 * 24 * time.Hour, Unit: 24 * time.Hour})

// Pick up a turn automatically after the app stopped mid-turn
var SessionsAutoContinueEnabled = Register(&Bool{Meta: Meta{Key: "sessions.auto_continue.enabled", Label: "Resume interrupted turns", Short: "Pick up a turn automatically after the app stopped mid-turn.", Group: GroupMemory, Order: 1090, Advanced: true}, Default: true})

// How recent an interrupted turn must be to resume itself
var SessionsAutoContinueFreshnessMinutes = Register(&Duration{Meta: Meta{Key: "sessions.auto_continue.freshness_minutes", Label: "Resume window", Short: "How recent an interrupted turn must be to resume itself.", Group: GroupMemory, Order: 1100, Advanced: true}, Default: 15 * time.Minute, Unit: time.Minute})

// Times one interrupted turn is resumed before giving up
var SessionsAutoContinueMaxAttempts = Register(&Int{Meta: Meta{Key: "sessions.auto_continue.max_attempts", Label: "Resume attempts", Short: "Times one interrupted turn is resumed before giving up.", Group: GroupMemory, Order: 1110, Advanced: true}, Default: 2, Min: at(0)})

// Make Chinese, Japanese and Korean text searchable in sessions
var SessionsCjkFts = Register(&Bool{Meta: Meta{Key: "sessions.cjk_fts", Label: "Search CJK text", Short: "Make Chinese, Japanese and Korean text searchable in sessions.", Group: GroupMemory, Order: 1120, Advanced: true}, Default: true})

// Whether to suggest tidying a large session search index
var SessionsFtsOptimizeNotice = Register(&Enum{Meta: Meta{Key: "sessions.fts_optimize_notice", Label: "Search tune-up notice", Short: "Whether to suggest tidying a large session search index.", Group: GroupMemory, Order: 1130, Advanced: true}, Default: "advise", Values: []Choice{
	{Value: "advise", Label: "Advise", Meaning: "Suggest it when the database is large."},
	{Value: "off", Label: "Off", Meaning: "Never suggest it."},
}})

// Largest session, in messages, that can be exported in memory
var SessionsMaxExportMessages = Register(&Int{Meta: Meta{Key: "sessions.max_export_messages", Label: "Export size limit", Short: "Largest session, in messages, that can be exported in memory.", Help: "Zero turns the guard off.", Group: GroupMemory, Order: 1140, Advanced: true}, Default: 20000, Unit: "messages"})

// Largest session, in messages, that can be resumed
var SessionsMaxResumeMessages = Register(&Int{Meta: Meta{Key: "sessions.max_resume_messages", Label: "Resume size limit", Short: "Largest session, in messages, that can be resumed.", Help: "Zero turns the guard off.", Group: GroupMemory, Order: 1150, Advanced: true}, Default: 20000, Unit: "messages"})

// Least time between automatic archive or cleanup runs
var SessionsMinIntervalHours = Register(&Duration{Meta: Meta{Key: "sessions.min_interval_hours", Label: "Cleanup spacing", Short: "Least time between automatic archive or cleanup runs.", Group: GroupMemory, Order: 1160, Advanced: true}, Default: 24 * time.Hour, Unit: time.Hour})

// Least time between compactions of the session database
var SessionsMinVacuumIntervalDays = Register(&Duration{Meta: Meta{Key: "sessions.min_vacuum_interval_days", Label: "Compact spacing", Short: "Least time between compactions of the session database.", Group: GroupMemory, Order: 1170, Advanced: true}, Default: 30 * 24 * time.Hour, Unit: 24 * time.Hour})

// How long an abandoned session waits before it is marked ended
var SessionsOrphanEndGraceS = Register(&Duration{Meta: Meta{Key: "sessions.orphan_end_grace_s", Label: "Abandoned session grace", Short: "How long an abandoned session waits before it is marked ended.", Help: "Zero ends it right away.", Group: GroupMemory, Order: 1180, Advanced: true}, Default: 5 * time.Minute})

// Age after which old sessions are deleted by automatic cleanup
var SessionsRetentionDays = Register(&Duration{Meta: Meta{Key: "sessions.retention_days", Label: "Keep old sessions", Short: "Age after which old sessions are deleted by automatic cleanup.", Help: "Applies only while Delete old sessions is on.", Group: GroupMemory, Order: 1190, Advanced: true}, Default: 90 * 24 * time.Hour, Unit: 24 * time.Hour})

// Searches slower than this are written to the log
var SessionsSearchSlowMs = Register(&Duration{Meta: Meta{Key: "sessions.search_slow_ms", Label: "Slow search mark", Short: "Searches slower than this are written to the log.", Group: GroupMemory, Order: 1200, Advanced: true}, Default: time.Second, Unit: time.Millisecond})

// Include sessions started by subagents in session lists
var SessionsShowSubagents = Register(&Bool{Meta: Meta{Key: "sessions.show_subagents", Label: "List subagent sessions", Short: "Include sessions started by subagents in session lists.", Group: GroupMemory, Order: 1210, Advanced: true}, Default: false})

// At startup, close sessions left open by a crash or shutdown
var SessionsStartupOrphanSweep = Register(&Bool{Meta: Meta{Key: "sessions.startup_orphan_sweep", Label: "Close stale sessions", Short: "At startup, close sessions left open by a crash or shutdown.", Group: GroupMemory, Order: 1220, Advanced: true}, Default: true})

// Shrink the session database after old sessions are deleted
var SessionsVacuumAfterPrune = Register(&Bool{Meta: Meta{Key: "sessions.vacuum_after_prune", Label: "Compact after cleanup", Short: "Shrink the session database after old sessions are deleted.", Group: GroupMemory, Order: 1230, Advanced: true}, Default: true})

// How long a disconnected session may sit idle before it is closed
var SessionsWsOrphanActivityStaleS = Register(&Duration{Meta: Meta{Key: "sessions.ws_orphan_activity_stale_s", Label: "Idle disconnect limit", Short: "How long a disconnected session may sit idle before it is closed.", Group: GroupMemory, Order: 1240, Advanced: true, Apply: Restart}, Default: 10 * time.Minute})

// How long a session is kept after its window disconnects
var SessionsWsOrphanReapGraceS = Register(&Duration{Meta: Meta{Key: "sessions.ws_orphan_reap_grace_s", Label: "Disconnect grace", Short: "How long a session is kept after its window disconnects.", Help: "Zero keeps it parked until it is reopened.", Group: GroupMemory, Order: 1250, Advanced: true, Apply: Restart}, Default: 20 * time.Second})

// Most sessions one window keeps open in the background service at once
var SessionsMaxPerWindow = Register(&Int{Meta: Meta{Key: "sessions.max_live_per_connection", Label: "Sessions per window", Short: "Most sessions one window keeps open at once.", Group: GroupMemory, Order: 1260, Advanced: true}, Default: 12, Min: at(1)})

// Most sessions the windows of one profile keep open in the background service at once
var SessionsMaxOpen = Register(&Int{Meta: Meta{Key: "sessions.max_live_per_profile", Label: "Open session limit", Short: "Most sessions open at once across every window of this profile.", Group: GroupMemory, Order: 1270, Advanced: true}, Default: 32, Min: at(1)})
