package settings

import "time"

// How long an unused agent stays loaded before it can be freed
var AgentAgentCacheIdleTtlSecs = Register(&Duration{Meta: Meta{Key: "agent.agent_cache.idle_ttl_secs", Label: "Idle session lifetime", Short: "How long an unused agent stays loaded before it can be freed.", Group: GroupSystem, Order: 1000, Advanced: true}, Default: time.Hour})

// Most idle agents freed in one cleanup sweep
var AgentAgentCacheMaxEvictionsPerPass = Register(&Int{Meta: Meta{Key: "agent.agent_cache.max_evictions_per_pass", Label: "Frees per sweep", Short: "Most idle agents freed in one cleanup sweep.", Group: GroupSystem, Order: 1010, Advanced: true}, Min: at(1), Default: 16, DefaultBelow: true})

// How many agents stay loaded before the oldest idle ones are freed
var AgentAgentCacheMaxSize = Register(&Int{Meta: Meta{Key: "agent.agent_cache.max_size", Label: "Loaded agents", Short: "How many agents stay loaded before the oldest idle ones are freed.", Group: GroupSystem, Order: 1020, Advanced: true}, Default: 128})

// Memory use in MB above which idle agents are freed
var AgentAgentCacheMemoryHighMb = Register(&String{Meta: Meta{Key: "agent.agent_cache.memory_high_mb", Label: "Memory ceiling", Short: "Memory use in MB above which idle agents are freed.", Help: "Use auto to pick from the machine's memory, a number of MB, or off to never free by memory.", Group: GroupSystem, Order: 1030, Advanced: true}, Default: "auto"})

// Most recently used agents that are never freed
var AgentAgentCacheProtectRecent = Register(&Int{Meta: Meta{Key: "agent.agent_cache.protect_recent", Label: "Keep recent agents", Short: "Most recently used agents that are never freed.", Group: GroupSystem, Order: 1040, Advanced: true}, Min: at(0), Default: 8, DefaultBelow: true})

// Times a failed model request is retried before giving up
var AgentApiMaxRetries = Register(&Int{Meta: Meta{Key: "agent.api_max_retries", Label: "Model retries", Short: "Times a failed model request is retried before giving up.", Group: GroupSystem, Order: 1050, Advanced: true, Apply: NewSession}, Default: 3})

// Times the agent tries to recover on its own after repeated failures
var AgentAutoRecoveryCycles = Register(&Int{Meta: Meta{Key: "agent.auto_recovery_cycles", Label: "Recovery attempts", Short: "Times the agent tries to recover on its own after repeated failures.", Group: GroupSystem, Order: 1060, Advanced: true, Apply: NewSession}, Default: 5})

// How long a first prompt waits for a new agent to finish loading
var AgentBuildWaitTimeout = Register(&Duration{Meta: Meta{Key: "agent.build_wait_timeout", Label: "Startup wait", Short: "How long a first prompt waits for a new agent to finish loading.", Group: GroupSystem, Order: 1070, Advanced: true}, Default: 10 * time.Minute})

// How long the agent waits for your answer to a question
var AgentClarifyTimeout = Register(&Duration{Meta: Meta{Key: "agent.clarify_timeout", Label: "Question timeout", Short: "How long the agent waits for your answer to a question.", Group: GroupSystem, Order: 1080, Advanced: true}, Default: time.Hour})

// How long a restart waits for running scheduled jobs to finish
var AgentCronDrainTimeout = Register(&Duration{Meta: Meta{Key: "agent.cron_drain_timeout", Label: "Scheduled job wait", Short: "How long a restart waits for running scheduled jobs to finish.", Group: GroupSystem, Order: 1090, Advanced: true, Apply: Restart}, Default: 30 * time.Second})

// How recent an interrupted chat app turn must be to resume itself
var AgentGatewayAutoContinueFreshness = Register(&Duration{Meta: Meta{Key: "agent.gateway_auto_continue_freshness", Label: "Resume window", Short: "How recent an interrupted chat app turn must be to resume itself.", Group: GroupSystem, Order: 1100, Advanced: true, Apply: Restart}, Default: time.Hour})

// How often a chat app is told a long turn is still running
var AgentGatewayNotifyInterval = Register(&Duration{Meta: Meta{Key: "agent.gateway_notify_interval", Label: "Still-working notice", Short: "How often a chat app is told a long turn is still running.", Help: "Zero turns the notices off.", Group: GroupSystem, Order: 1110, Advanced: true, Apply: Restart}, Default: 3 * time.Minute})

// How long startup waits for interrupted turns to be restored
var AgentGatewayStartupRestoreDrainTimeout = Register(&Duration{Meta: Meta{Key: "agent.gateway_startup_restore_drain_timeout", Label: "Startup restore wait", Short: "How long startup waits for interrupted turns to be restored.", Group: GroupSystem, Order: 1120, Advanced: true, Apply: Restart}, Default: 30 * time.Second})

// How long startup waits for the background service to warm up
var AgentGatewayStartupWarmupTimeout = Register(&Duration{Meta: Meta{Key: "agent.gateway_startup_warmup_timeout", Label: "Startup warmup wait", Short: "How long startup waits for the background service to warm up.", Group: GroupSystem, Order: 1130, Advanced: true, Apply: Restart}, Default: 20 * time.Second})

// How long a chat app turn may run before it is stopped
var AgentGatewayTimeout = Register(&Duration{Meta: Meta{Key: "agent.gateway_timeout", Label: "Chat turn timeout", Short: "How long a chat app turn may run before it is stopped.", Group: GroupSystem, Order: 1140, Advanced: true, Apply: Restart}, Default: 30 * time.Minute})

// How far into a chat turn a timeout warning is sent
var AgentGatewayTimeoutWarning = Register(&Duration{Meta: Meta{Key: "agent.gateway_timeout_warning", Label: "Chat turn warning", Short: "How far into a chat turn a timeout warning is sent.", Group: GroupSystem, Order: 1150, Advanced: true, Apply: Restart}, Default: 15 * time.Minute})

// How long a chat turn waits for the session to be free
var AgentGatewayTurnLeaseTimeout = Register(&Duration{Meta: Meta{Key: "agent.gateway_turn_lease_timeout", Label: "Turn lock timeout", Short: "How long a chat turn waits for the session to be free.", Group: GroupSystem, Order: 1160, Advanced: true, Apply: Restart}, Default: 5 * time.Second})

// How long a local model may stay silent before the request is dropped
var AgentLocalStreamStaleTimeout = Register(&Duration{Meta: Meta{Key: "agent.local_stream_stale_timeout", Label: "Local silence limit", Short: "How long a local model may stay silent before the request is dropped.", Help: "Zero means no limit.", Group: GroupSystem, Order: 1170, Advanced: true}, Default: 15 * time.Minute, Min: at(time.Duration(0))})

// How long a chat app can stay disconnected before you are alerted
var AgentReconnectAttentionAfter = Register(&Duration{Meta: Meta{Key: "agent.reconnect_attention_after", Label: "Reconnect alert delay", Short: "How long a chat app can stay disconnected before you are alerted.", Help: "Zero or less turns the alert off.", Group: GroupSystem, Order: 1180, Advanced: true}, Default: 2 * time.Hour})

// How long a restart waits for running turns before going ahead
var AgentRestartAfterTurnTimeout = Register(&Duration{Meta: Meta{Key: "agent.restart_after_turn_timeout", Label: "Restart wait", Short: "How long a restart waits for running turns before going ahead.", Group: GroupSystem, Order: 1190, Advanced: true, Apply: Restart}, Default: 30 * time.Minute})

// How long shutdown waits for running work to wind down
var AgentRestartDrainTimeout = Register(&Duration{Meta: Meta{Key: "agent.restart_drain_timeout", Label: "Restart drain wait", Short: "How long shutdown waits for running work to wind down.", Help: "Zero uses the built-in wait.", Group: GroupSystem, Order: 1200, Advanced: true, Apply: Restart}, Default: 0})

// Message repairs in a row before the agent reports a problem
var AgentSanitizerHealEscalationThreshold = Register(&Int{Meta: Meta{Key: "agent.sanitizer_heal_escalation_threshold", Label: "Repair limit", Short: "Message repairs in a row before the agent reports a problem.", Group: GroupSystem, Order: 1210, Advanced: true}, Default: 3})

// How long a chat session may sit with no activity before it is stuck
var AgentSessionStallTimeout = Register(&Duration{Meta: Meta{Key: "agent.session_stall_timeout", Label: "Stall timeout", Short: "How long a chat session may sit with no activity before it is stuck.", Group: GroupSystem, Order: 1220, Advanced: true, Apply: Restart}, Default: 5 * time.Minute})

// How long to wait for a finished model stream to close
var AgentStreamDrainTimeout = Register(&Duration{Meta: Meta{Key: "agent.stream_drain_timeout", Label: "Stream close wait", Short: "How long to wait for a finished model stream to close.", Group: GroupSystem, Order: 1230, Advanced: true}, Default: 2 * time.Second, Min: at(time.Duration(0))})

// Longest a single model response may keep streaming
var AgentStreamMaxLifetime = Register(&Duration{Meta: Meta{Key: "agent.stream_max_lifetime", Label: "Stream time limit", Short: "Longest a single model response may keep streaming.", Help: "Zero means no limit.", Group: GroupSystem, Order: 1240, Advanced: true}, Default: 0, Min: at(time.Duration(0))})

// How long a model may stay silent before the request is retried
var AgentStreamStaleTimeout = Register(&Duration{Meta: Meta{Key: "agent.stream_stale_timeout", Label: "Model silence limit", Short: "How long a model may stay silent before the request is retried.", Help: "Zero means no limit.", Group: GroupSystem, Order: 1250, Advanced: true}, Default: 3 * time.Minute, Min: at(time.Duration(0))})

// How often a running turn is checked for being stuck
var AgentTurnLivenessPollS = Register(&Duration{Meta: Meta{Key: "agent.turn_liveness.poll_s", Label: "Hang check interval", Short: "How often a running turn is checked for being stuck.", Group: GroupSystem, Order: 1260, Advanced: true}, Default: 15 * time.Second})

// How long a turn can make no progress before it is flagged as stuck
var AgentTurnLivenessTimeoutS = Register(&Duration{Meta: Meta{Key: "agent.turn_liveness.timeout_s", Label: "Hang timeout", Short: "How long a turn can make no progress before it is flagged as stuck.", Help: "Zero or less turns the check off.", Group: GroupSystem, Order: 1270, Advanced: true}, Default: 10 * time.Minute})

// Minimum time between automatic memory cleanups
var ContextMemoryTrimCooldownSeconds = Register(&Duration{Meta: Meta{Key: "context.memory_trim.cooldown_seconds", Label: "Memory trim cooldown", Short: "Minimum time between automatic memory cleanups.", Group: GroupSystem, Order: 1280, Advanced: true}, Default: time.Minute})

// Give unused process memory back to the system after agents close
var ContextMemoryTrimEnabled = Register(&Bool{Meta: Meta{Key: "context.memory_trim.enabled", Label: "Trim memory use", Short: "Give unused process memory back to the system after agents close.", Group: GroupSystem, Order: 1290, Advanced: true}, Default: true})

// Smallest memory cleanup, in MB, worth writing to the log
var ContextMemoryTrimInfoLogMinDeltaMb = Register(&Float{Meta: Meta{Key: "context.memory_trim.info_log_min_delta_mb", Label: "Trim log threshold", Short: "Smallest memory cleanup, in MB, worth writing to the log.", Group: GroupSystem, Order: 1300, Advanced: true}, Default: 0, Unit: "MB", Min: at(0.0)})

// Log only every Nth memory cleanup
var ContextMemoryTrimLogEveryN = Register(&Int{Meta: Meta{Key: "context.memory_trim.log_every_n", Label: "Trim log spacing", Short: "Log only every Nth memory cleanup.", Group: GroupSystem, Order: 1310, Advanced: true}, Default: 1, Min: at(1)})

// How the session database writes to disk
var DatabaseJournalMode = Register(&Enum{Meta: Meta{Key: "database.journal_mode", Label: "Database journal mode", Short: "How the session database writes to disk.", Help: "Use delete on network drives or file systems that do not support write-ahead logs.", Group: GroupSystem, Order: 1320, Advanced: true, Apply: Restart}, Default: "wal", Values: []Choice{
	{Value: "wal", Label: "Write-ahead log", Meaning: "Faster, readers do not block writers."},
	{Value: "delete", Label: "Delete", Meaning: "Slower, works on network drives."},
}})

// Largest size the database write log may keep after cleanup
var DatabaseJournalSizeLimit = Register(&Int{Meta: Meta{Key: "database.journal_size_limit", Label: "Journal size cap", Short: "Largest size the database write log may keep after cleanup.", Group: GroupSystem, Order: 1330, Advanced: true, Apply: Restart}, Unit: "bytes", Null: "database default"})

// Pages written before the database log is merged back
var DatabaseWalAutocheckpoint = Register(&Int{Meta: Meta{Key: "database.wal_autocheckpoint", Label: "Checkpoint interval", Short: "Pages written before the database log is merged back.", Group: GroupSystem, Order: 1340, Advanced: true, Apply: Restart}, Unit: "pages", Null: "database default"})

// How long each live connection check in the doctor may take
var DoctorLiveProbeTimeout = Register(&Duration{Meta: Meta{Key: "doctor.live_probe_timeout", Label: "Live check timeout", Short: "How long each live connection check in the doctor may take.", Group: GroupSystem, Order: 1350, Advanced: true}, Default: 10 * time.Second, Min: at(time.Second)})

// Which engine runs local models
var LocalRuntimeBackend = Register(&String{Meta: Meta{Key: "local_runtime.backend", Label: "Local model engine", Short: "Which engine runs local models.", Help: "Auto picks one for this machine.", Group: GroupSystem, Order: 1360, Advanced: true}, Default: "auto"})

// Ports checked for an already running local model server
var LocalRuntimeDetectPorts = Register(&List{Meta: Meta{Key: "local_runtime.detect_ports", Label: "Local server ports", Short: "Ports checked for an already running local model server.", Group: GroupSystem, Order: 1370, Advanced: true}})

// Start and manage a local model server automatically
var LocalRuntimeEnabled = Register(&Bool{Meta: Meta{Key: "local_runtime.enabled", Label: "Run local models", Short: "Start and manage a local model server automatically.", Group: GroupSystem, Order: 1380, Advanced: true}, Default: false})

// Most local models kept loaded at once
var LocalRuntimeModelsMax = Register(&Int{Meta: Meta{Key: "local_runtime.models_max", Label: "Local models loaded", Short: "Most local models kept loaded at once.", Help: "The real limit can be lower when the graphics card cannot fit this many.", Group: GroupSystem, Order: 1390, Advanced: true}, Default: 4})

// Port the local model server listens on
var LocalRuntimePort = Register(&Int{Meta: Meta{Key: "local_runtime.port", Label: "Local server port", Short: "Port the local model server listens on.", Help: "Zero lets the system choose a free port.", Group: GroupSystem, Order: 1400, Advanced: true}, Default: 0})

// How many rotated copies of each log file are kept
var LoggingBackupCount = Register(&Int{Meta: Meta{Key: "logging.backup_count", Label: "Old logs kept", Short: "How many rotated copies of each log file are kept.", Group: GroupSystem, Order: 1410, Advanced: true, Apply: Restart}, Default: 3})

// How much the log files record
var LoggingLevel = Register(&Enum{Meta: Meta{Key: "logging.level", Label: "Log detail", Short: "How much the log files record.", Group: GroupSystem, Order: 1420, Advanced: true, Apply: Restart}, Default: "INFO", Values: []Choice{
	{Value: "DEBUG", Label: "Debug", Meaning: "Everything, for troubleshooting."},
	{Value: "INFO", Label: "Info", Meaning: "Normal activity and problems."},
	{Value: "WARNING", Label: "Warning", Meaning: "Only warnings and errors."},
	{Value: "ERROR", Label: "Error", Meaning: "Only errors."},
	{Value: "CRITICAL", Label: "Critical", Meaning: "Only fatal problems."},
}})

// Size at which a log file is rotated
var LoggingMaxSizeMb = Register(&Int{Meta: Meta{Key: "logging.max_size_mb", Label: "Log file size", Short: "Size at which a log file is rotated.", Group: GroupSystem, Order: 1430, Advanced: true, Apply: Restart}, Default: 5, Unit: "MB"})

// How long to wait before retrying a code server that failed
var LspBrokenRetrySeconds = Register(&Duration{Meta: Meta{Key: "lsp.broken_retry_seconds", Label: "Retry failed servers", Short: "How long to wait before retrying a code server that failed.", Help: "Zero never retries until restart.", Group: GroupSystem, Order: 1440, Advanced: true}, Default: 0})

// Check edited files for problems using language servers
var LspEnabled = Register(&Bool{Meta: Meta{Key: "lsp.enabled", Label: "Code checking", Short: "Check edited files for problems using language servers.", Group: GroupSystem, Order: 1450, Advanced: true}, Default: true})

// Folders, as glob patterns, where code checking is never used
var LspExcludeRoots = Register(&List{Meta: Meta{Key: "lsp.exclude_roots", Label: "Skip folders", Short: "Folders, as glob patterns, where code checking is never used.", Group: GroupSystem, Order: 1460, Advanced: true}})

// How long an unused language server runs before it is stopped
var LspIdleTimeout = Register(&Duration{Meta: Meta{Key: "lsp.idle_timeout", Label: "Server idle time", Short: "How long an unused language server runs before it is stopped.", Group: GroupSystem, Order: 1470, Advanced: true}, Default: 10 * time.Minute})

// Whether missing language servers are installed for you
var LspInstallStrategy = Register(&Enum{Meta: Meta{Key: "lsp.install_strategy", Label: "Install language servers", Short: "Whether missing language servers are installed for you.", Group: GroupSystem, Order: 1480, Advanced: true}, Default: "auto", Values: []Choice{
	{Value: "auto", Label: "Automatic", Meaning: "Install missing servers when needed."},
	{Value: "manual", Label: "Manual", Meaning: "Use only servers you installed."},
}})

// Tool used to install language servers written for Node
var LspPackageManager = Register(&Enum{Meta: Meta{Key: "lsp.package_manager", Label: "Node package manager", Short: "Tool used to install language servers written for Node.", Group: GroupSystem, Order: 1490, Advanced: true}, Default: "npm", Values: []Choice{
	{Value: "npm", Label: "npm", Meaning: "Use npm."},
	{Value: "pnpm", Label: "pnpm", Meaning: "Use pnpm."},
	{Value: "yarn", Label: "yarn", Meaning: "Use yarn."},
}})

// Per-server overrides such as custom commands or disabling one
var LspServers = Register(&Map{Meta: Meta{Key: "lsp.servers", Label: "Language servers", Short: "Per-server overrides such as custom commands or disabling one.", Group: GroupSystem, Order: 1500, Advanced: true}})

// Extra folders where project-defined checkers may run
var LspTrustedWorkspaces = Register(&List{Meta: Meta{Key: "lsp.trusted_workspaces", Label: "Trusted folders", Short: "Extra folders where project-defined checkers may run.", Group: GroupSystem, Order: 1510, Advanced: true}})

// How much to wait for after an edit before reporting problems
var LspWaitMode = Register(&Enum{Meta: Meta{Key: "lsp.wait_mode", Label: "Check wait mode", Short: "How much to wait for after an edit before reporting problems.", Group: GroupSystem, Order: 1520, Advanced: true}, Default: "document", Values: []Choice{
	{Value: "document", Label: "Edited file", Meaning: "Shorter wait for the edited file."},
	{Value: "full", Label: "Full", Meaning: "Longer wait for a fuller result."},
}})

// Longest wait for code problems after an edit
var LspWaitTimeout = Register(&Duration{Meta: Meta{Key: "lsp.wait_timeout", Label: "Check wait time", Short: "Longest wait for code problems after an edit.", Group: GroupSystem, Order: 1530, Advanced: true}, Default: 5 * time.Second})

// Extra time a new language server gets to start before checks
var LspWarmupTimeout = Register(&Duration{Meta: Meta{Key: "lsp.warmup_timeout", Label: "Code server warmup", Short: "Extra time a new language server gets to start before checks.", Help: "Zero means no extra wait.", Group: GroupSystem, Order: 1540, Advanced: true}, Default: 0})

// Most chat sessions that may run a turn at once
var MaxConcurrentSessions = Register(&Int{Meta: Meta{Key: "max_concurrent_sessions", Label: "Active session limit", Short: "Most chat sessions that may run a turn at once.", Group: GroupSystem, Order: 1550, Advanced: true, Apply: Restart}, Null: "no limit"})

// Most sessions kept open before the least recent are closed
var MaxLiveSessions = Register(&Int{Meta: Meta{Key: "max_live_sessions", Label: "Live session limit", Short: "Most sessions kept open before the least recent are closed.", Help: "Zero removes the limit.", Group: GroupSystem, Order: 1560, Advanced: true}, Default: 16})

// Fetch the list of available models from the online catalog
var ModelCatalogEnabled = Register(&Bool{Meta: Meta{Key: "model_catalog.enabled", Label: "Online model list", Short: "Fetch the list of available models from the online catalog.", Group: GroupSystem, Order: 1570, Advanced: true}, Default: true})

// Providers left out of the model picker
var ModelCatalogExcludedProviders = Register(&List{Meta: Meta{Key: "model_catalog.excluded_providers", Label: "Hidden providers", Short: "Providers left out of the model picker.", Group: GroupSystem, Order: 1580, Advanced: true}})

// Per-provider changes applied to the online model list
var ModelCatalogProviders = Register(&Map{Meta: Meta{Key: "model_catalog.providers", Label: "Provider overrides", Short: "Per-provider changes applied to the online model list.", Group: GroupSystem, Order: 1590, Advanced: true}})

// How often the online model list is refreshed
var ModelCatalogTtlMinutes = Register(&Duration{Meta: Meta{Key: "model_catalog.ttl_minutes", Label: "Model list refresh", Short: "How often the online model list is refreshed.", Group: GroupSystem, Order: 1600, Advanced: true}, Default: 20 * time.Minute, Unit: time.Minute})

// Web address the online model list is fetched from
var ModelCatalogUrl = Register(&String{Meta: Meta{Key: "model_catalog.url", Label: "Model list address", Short: "Web address the online model list is fetched from.", Group: GroupSystem, Order: 1610, Advanced: true}, Default: "https://icebath.app/api/model-catalog.json"})

// Web address for model details such as limits and prices
var ModelsDevUrl = Register(&String{Meta: Meta{Key: "models_dev.url", Label: "Model details address", Short: "Web address for model details such as limits and prices.", Help: "Empty uses the public models.dev registry.", Group: GroupSystem, Order: 1620, Advanced: true}, Default: ""})

// Send traces to an OpenTelemetry collector
var MonitoringExportOtlpEnabled = Register(&Bool{Meta: Meta{Key: "monitoring.export.otlp.enabled", Label: "Send telemetry", Short: "Send traces to an OpenTelemetry collector.", Help: "Needs an address under the collector endpoint setting.", Group: GroupSystem, Order: 1630, Advanced: true}, Default: false})

// Web address of the OpenTelemetry collector
var MonitoringExportOtlpEndpoint = Register(&String{Meta: Meta{Key: "monitoring.export.otlp.endpoint", Label: "Collector address", Short: "Web address of the OpenTelemetry collector.", Group: GroupSystem, Order: 1640, Advanced: true}, Default: ""})

// Request headers for the collector, each read from a named variable
var MonitoringExportOtlpHeadersEnv = Register(&Map{Meta: Meta{Key: "monitoring.export.otlp.headers_env", Label: "Collector headers", Short: "Request headers for the collector, each read from a named variable.", Group: GroupSystem, Order: 1650, Advanced: true}})

// Include diagnostic events in the background service health export
var MonitoringGatewayHealthExportDiagnosticEventsEnabled = Register(&Bool{Meta: Meta{Key: "monitoring.gateway_health_export.diagnostic_events_enabled", Label: "Send diagnostic events", Short: "Include diagnostic events in the background service health export.", Group: GroupSystem, Order: 1660, Advanced: true}, Default: true})

// Send background service health data to the telemetry collector
var MonitoringGatewayHealthExportEnabled = Register(&Bool{Meta: Meta{Key: "monitoring.gateway_health_export.enabled", Label: "Export service health", Short: "Send background service health data to the telemetry collector.", Group: GroupSystem, Order: 1670, Advanced: true}, Default: false})

// How often health metrics are sent
var MonitoringGatewayHealthExportExportIntervalSeconds = Register(&Duration{Meta: Meta{Key: "monitoring.gateway_health_export.export_interval_seconds", Label: "Metrics interval", Short: "How often health metrics are sent.", Group: GroupSystem, Order: 1680, Advanced: true}, Default: time.Minute})

// How often warning and error logs are sent
var MonitoringGatewayHealthExportLogsExportIntervalSeconds = Register(&Duration{Meta: Meta{Key: "monitoring.gateway_health_export.logs_export_interval_seconds", Label: "Log export interval", Short: "How often warning and error logs are sent.", Group: GroupSystem, Order: 1690, Advanced: true}, Default: 5 * time.Second})

// Include numeric health metrics in the export
var MonitoringGatewayHealthExportMetricsEnabled = Register(&Bool{Meta: Meta{Key: "monitoring.gateway_health_export.metrics_enabled", Label: "Send health metrics", Short: "Include numeric health metrics in the export.", Group: GroupSystem, Order: 1700, Advanced: true}, Default: true})

// Include warning and error log events in the export
var MonitoringGatewayHealthExportWarningErrorEventsEnabled = Register(&Bool{Meta: Meta{Key: "monitoring.gateway_health_export.warning_error_events_enabled", Label: "Send warning logs", Short: "Include warning and error log events in the export.", Group: GroupSystem, Order: 1730, Advanced: true}, Default: true})

// Anonymous ID that labels this install in exported data
var MonitoringInstallId = Register(&String{Meta: Meta{Key: "monitoring.install_id", Label: "Install ID", Short: "Anonymous ID that labels this install in exported data.", Help: "Created automatically when empty.", Group: GroupSystem, Order: 1740, Advanced: true}, Default: ""})

// Try IPv4 addresses first, for networks with broken IPv6
var NetworkForceIpv4 = Register(&Bool{Meta: Meta{Key: "network.force_ipv4", Label: "Prefer IPv4", Short: "Try IPv4 addresses first, for networks with broken IPv6.", Group: GroupSystem, Order: 1750, Advanced: true, Apply: Restart}, Default: false})

// Whether the agent offers to build your profile
var OnboardingProfileBuild = Register(&Enum{Meta: Meta{Key: "onboarding.profile_build", Label: "Profile setup offer", Short: "Whether the agent offers to build your profile.", Group: GroupSystem, Order: 1760, Advanced: true}, Default: "ask", Values: []Choice{
	{Value: "ask", Label: "Ask", Meaning: "Offer once during first use."},
	{Value: "off", Label: "Off", Meaning: "Never offer."},
}})

// Which first-run tips you have already seen
var OnboardingSeen = Register(&Map{Meta: Meta{Key: "onboarding.seen", Label: "Seen tips", Short: "Which first-run tips you have already seen.", Help: "Filled in automatically.", Group: GroupSystem, Order: 1770, Advanced: true}})

// Cap on memory pools the system allocator creates
var RuntimeMallocArenaMax = Register(&Int{Meta: Meta{Key: "runtime.malloc_arena_max", Label: "Memory arenas", Short: "Cap on memory pools the system allocator creates.", Help: "Lower values reduce memory growth on Linux.", Group: GroupSystem, Order: 1780, Advanced: true, Apply: Restart}, Default: 4})

// Open files the background service asks the system to allow
var RuntimeNofileSoftLimit = Register(&Int{Meta: Meta{Key: "runtime.nofile_soft_limit", Label: "Open file limit", Short: "Open files the background service asks the system to allow.", Group: GroupSystem, Order: 1790, Advanced: true, Apply: Restart}, Default: 4096})

// Return to the update branch when updating from another one
var UpdatesAutoSwitchParkedBranch = Register(&Bool{Meta: Meta{Key: "updates.auto_switch_parked_branch", Label: "Switch on update", Short: "Return to the update branch when updating from another one.", Group: GroupSystem, Order: 1800, Advanced: true}, Default: true})

// How many full pre-update backups are kept
var UpdatesBackupKeep = Register(&Int{Meta: Meta{Key: "updates.backup_keep", Label: "Backups kept", Short: "How many full pre-update backups are kept.", Group: GroupSystem, Order: 1810, Advanced: true}, Default: 5, Unit: "backups"})

// Look for new versions in the background
var UpdatesCheck = Register(&Bool{Meta: Meta{Key: "updates.check", Label: "Check for updates", Short: "Look for new versions in the background.", Group: GroupSystem, Order: 1820, Advanced: true}, Default: true})

// What an unattended update does with your changes to the source
var UpdatesNonInteractiveLocalChanges = Register(&Enum{Meta: Meta{Key: "updates.non_interactive_local_changes", Label: "Unattended local edits", Short: "What an unattended update does with your changes to the source.", Group: GroupSystem, Order: 1830, Advanced: true}, Default: "stash", Values: []Choice{
	{Value: "stash", Label: "Stash", Meaning: "Set changes aside and restore after."},
	{Value: "discard", Label: "Discard", Meaning: "Throw the changes away."},
}})

// What an update does when a non-default branch is checked out
var UpdatesParkedBranchStrategy = Register(&Enum{Meta: Meta{Key: "updates.parked_branch_strategy", Label: "Other-branch update", Short: "What an update does when a non-default branch is checked out.", Group: GroupSystem, Order: 1840, Advanced: true}, Default: "switch", Values: []Choice{
	{Value: "switch", Label: "Switch", Meaning: "Move to the update branch."},
	{Value: "update_in_place", Label: "Update in place", Meaning: "Stay on your branch and update it."},
}})

// What is backed up before an update runs
var UpdatesPreUpdateBackup = Register(&Enum{Meta: Meta{Key: "updates.pre_update_backup", Label: "Backup before update", Short: "What is backed up before an update runs.", Group: GroupSystem, Order: 1850, Advanced: true}, Default: "quick", Also: map[string]string{"false": "off", "none": "off", "disabled": "off", "true": "full", "zip": "full"}, Values: []Choice{
	{Value: "off", Label: "Off", Meaning: "No backup."},
	{Value: "quick", Label: "Quick", Meaning: "Snapshot of state only."},
	{Value: "full", Label: "Full", Meaning: "Zip of the whole home folder."},
}})

// Update the computer-control helper along with the app
var UpdatesRefreshCuaDriver = Register(&Bool{Meta: Meta{Key: "updates.refresh_cua_driver", Label: "Refresh computer driver", Short: "Update the computer-control helper along with the app.", Group: GroupSystem, Order: 1860, Advanced: true}, Default: true})

// Labels attached to exported health data, only service, cloud and environment names kept
var MonitoringGatewayHealthExportResourceAttributes = Register(&Map{Meta: Meta{Key: "monitoring.gateway_health_export.resource_attributes", Label: "Export labels", Short: "Labels such as an environment name attached to exported health data.", Help: "Only service, cloud and environment names are sent; the service name is always icebath-gateway.", Group: GroupSystem, Order: 1705, Advanced: true}, Default: map[string]any{"deployment.environment.name": "production", "service.name": "icebath-gateway"}})

// Pages of the session database kept in memory
var DatabaseCacheSize = Register(&Int{Meta: Meta{Key: "database.cache_size", Label: "Database cache", Short: "Memory the session database keeps for recent pages.", Help: "A positive number counts pages, a negative one counts KiB.", Group: GroupSystem, Order: 1342, Advanced: true, Apply: Restart}, Null: "database default"})

// Bytes of the session database read through memory mapping
var DatabaseMmapSize = Register(&Int{Meta: Meta{Key: "database.mmap_size", Label: "Database memory map", Short: "How much of the session database is read through memory mapping.", Group: GroupSystem, Order: 1344, Advanced: true, Apply: Restart}, Null: "database default", Min: at(0), Unit: "bytes"})

// Where the session database keeps its temporary tables
var DatabaseTempStore = Register(&Enum{Meta: Meta{Key: "database.temp_store", Label: "Database temp storage", Short: "Where the session database keeps temporary tables.", Group: GroupSystem, Order: 1346, Advanced: true, Apply: Restart}, Null: "database default", Values: []Choice{{Value: "0", Label: "Default", Meaning: "As the database was built."}, {Value: "1", Label: "File", Meaning: "In temporary files."}, {Value: "2", Label: "Memory", Meaning: "In memory."}}})

// How carefully the session database waits for the disk after each write
var DatabaseSynchronous = Register(&Enum{Meta: Meta{Key: "database.synchronous", Label: "Database write safety", Short: "How carefully the session database waits for the disk after writes.", Help: "macOS always keeps Full.", Group: GroupSystem, Order: 1348, Advanced: true, Apply: Restart}, Null: "database default", Values: []Choice{{Value: "OFF", Label: "Off", Meaning: "Fastest, may lose data on a crash."}, {Value: "NORMAL", Label: "Normal", Meaning: "Safe in most crashes."}, {Value: "FULL", Label: "Full", Meaning: "Waits for every write."}, {Value: "EXTRA", Label: "Extra", Meaning: "Also waits for the journal."}}})

// How long one model request may take before it is given up
var AgentApiTimeout = Register(&Duration{Meta: Meta{Key: "agent.api_timeout", Label: "Request time limit", Short: "How long one model request may take before it is given up.", Help: "A provider's own request timeout under Providers wins over this.", Group: GroupSystem, Order: 3050, Advanced: true, Apply: NextTurn}, Default: 1800 * time.Second, Min: at(time.Second)})

// How long a model may stay silent on a request that is not streamed
var AgentApiCallStaleTimeout = Register(&Duration{Meta: Meta{Key: "agent.api_call_stale_timeout", Label: "Silent request limit", Short: "How long a model may stay silent on a request that is not streamed.", Help: "Unset picks it from the model and the request size. Zero means no limit. A provider's own stale timeout wins over this.", Group: GroupSystem, Order: 3060, Advanced: true, Apply: NextTurn}, Null: "picked from the model and request size", Min: at(time.Duration(0))})

// Longest a batch of tool calls run together may take
var TimeoutsToolsConcurrentBatch = Register(&Duration{Meta: Meta{Key: "timeouts.tools.concurrent_batch", Label: "Tool batch limit", Short: "Longest a batch of tool calls run at the same time may take.", Help: "Zero means no limit.", Group: GroupSystem, Order: 2000, Advanced: true, Apply: NextTurn}, Default: 7 * time.Minute, Min: at(time.Duration(0))})

// Longest one tool call run on its own may take
var TimeoutsToolsSequentialCall = Register(&Duration{Meta: Meta{Key: "timeouts.tools.sequential_call", Label: "Single tool limit", Short: "Longest one tool call run on its own may take.", Help: "Zero means no limit.", Group: GroupSystem, Order: 2010, Advanced: true, Apply: NextTurn}, Null: "the tool batch limit", Min: at(time.Duration(0))})

// Longest the terminal UI waits for the background service to answer a request
var DisplayTuiRequestTimeout = Register(&Duration{Meta: Meta{Key: "display.tui_request_timeout", Label: "Request timeout", Short: "Longest the terminal UI waits for the background service to answer.", Group: GroupSystem, Order: 2100, Advanced: true, Apply: NewSession, Env: "ICEBATH_TUI_RPC_TIMEOUT_MS"}, Default: 2 * time.Minute, Unit: time.Millisecond, Min: at(30 * time.Second)})

// Longest the terminal UI waits for the background service to be ready
var DisplayTuiStartupTimeout = Register(&Duration{Meta: Meta{Key: "display.tui_startup_timeout", Label: "Startup timeout", Short: "Longest the terminal UI waits for the background service to be ready.", Group: GroupSystem, Order: 2110, Advanced: true, Apply: NewSession, Env: "ICEBATH_TUI_STARTUP_TIMEOUT_MS"}, Default: 15 * time.Second, Unit: time.Millisecond, Min: at(5 * time.Second)})

// File the terminal UI writes a CPU profile to while it runs
var DiagnosticsCPUProfile = Register(&Path{Meta: Meta{Key: "diagnostics.cpu_profile", Label: "CPU profile file", Short: "File the terminal UI writes a CPU profile to while it runs.", Help: "Empty writes none.", Group: GroupSystem, Order: 2120, Advanced: true, Apply: NewSession, Env: "ICEBATH_CPUPROFILE"}, Default: ""})

// Frames a second the terminal UI draws, shown over the window
var DiagnosticsFrameRate = Register(&Bool{Meta: Meta{Key: "diagnostics.frame_rate", Label: "Frame rate readout", Short: "Show how many frames a second the terminal UI draws.", Group: GroupSystem, Order: 2130, Advanced: true, Apply: NewSession, Env: "ICEBATH_TUI_FPS"}, Default: false})

// Log of how long the terminal UI takes to draw its slow frames
var DiagnosticsFrameLog = Register(&Bool{Meta: Meta{Key: "diagnostics.frame_log", Label: "Frame timing log", Short: "Log how long the terminal UI takes to draw its slow frames.", Group: GroupSystem, Order: 2140, Advanced: true, Apply: NewSession, Env: "ICEBATH_DEV_PERF"}, Default: false})

// Frames drawn faster than this stay out of the frame timing log
var DiagnosticsFrameLogThreshold = Register(&Duration{Meta: Meta{Key: "diagnostics.frame_log_threshold", Label: "Slow frame time", Short: "Frames drawn faster than this stay out of the frame timing log.", Help: "Zero logs every frame.", Group: GroupSystem, Order: 2150, Advanced: true, Apply: NewSession, Env: "ICEBATH_DEV_PERF_MS"}, Default: 2 * time.Millisecond, Unit: time.Millisecond, Min: at(time.Duration(0))})

// File the frame timing log is written to
var DiagnosticsFrameLogFile = Register(&Path{Meta: Meta{Key: "diagnostics.frame_log_file", Label: "Frame log file", Short: "File the frame timing log is written to.", Help: "Empty writes perf.log in the icebath folder.", Group: GroupSystem, Order: 2160, Advanced: true, Apply: NewSession, Env: "ICEBATH_DEV_PERF_LOG"}, Default: ""})

// Folder the memory dumps of the terminal UI go to
var DiagnosticsHeapdumpDir = Register(&Path{Meta: Meta{Key: "diagnostics.heapdump_dir", Label: "Memory dump folder", Short: "Folder the memory dumps of the terminal UI go to.", Help: "Empty uses the heapdumps folder in the icebath folder.", Group: GroupSystem, Order: 2170, Advanced: true, Apply: NewSession, Env: "ICEBATH_HEAPDUMP_DIR"}, Default: ""})

// Whole heap profiles written by the memory watchdog, not only the memory facts
var DiagnosticsAutoHeapProfile = Register(&Bool{Meta: Meta{Key: "diagnostics.auto_heap_profile", Label: "Automatic heap profiles", Short: "Let the memory watchdog write whole heap profiles, not only facts.", Help: "Heap profiles of a large heap take much disk space.", Group: GroupSystem, Order: 2180, Advanced: true, Apply: NewSession, Env: "ICEBATH_AUTO_HEAPDUMP"}, Default: false})

// Space the memory dump folder may take before the oldest dumps are deleted
var DiagnosticsHeapdumpMaxBytes = Register(&Int{Meta: Meta{Key: "diagnostics.heapdump_max_bytes", Label: "Memory dump space", Short: "Space the memory dumps may take before the oldest are deleted.", Help: "The newest dump is always kept.", Group: GroupSystem, Order: 2190, Advanced: true, Apply: NewSession, Env: "ICEBATH_HEAPDUMP_MAX_BYTES"}, Default: 2 << 30, Min: at(1), Unit: "bytes"})

// Shortest wait between two automatic memory dumps
var DiagnosticsHeapdumpCooldown = Register(&Duration{Meta: Meta{Key: "diagnostics.heapdump_cooldown", Label: "Memory dump pause", Short: "Shortest wait between two automatic memory dumps.", Help: "A memory dump just before the window closes for lack of memory is never held back.", Group: GroupSystem, Order: 2200, Advanced: true, Apply: NewSession, Env: "ICEBATH_AUTO_HEAPDUMP_COOLDOWN_MS"}, Default: 10 * time.Minute, Unit: time.Millisecond, Min: at(time.Duration(0))})

// Memory dump written as soon as the terminal UI starts
var DiagnosticsHeapdumpOnStart = Register(&Bool{Meta: Meta{Key: "diagnostics.heapdump_on_start", Label: "Dump memory at start", Short: "Write a memory dump as soon as the terminal UI starts.", Group: GroupSystem, Order: 2210, Advanced: true, Apply: NewSession, Env: "ICEBATH_HEAPDUMP_ON_START"}, Default: false})

// Longest the command line's cleanup may take before the process ends anyway
var ShutdownExitWatchdog = Register(&Duration{Meta: Meta{Key: "shutdown.exit_watchdog", Label: "Exit watchdog", Short: "Longest the command line's cleanup may take before it is ended anyway.", Help: "A stop signal doubles it. Zero turns the watchdog off.", Group: GroupSystem, Order: 2220, Advanced: true, Apply: NewSession, Env: "ICEBATH_EXIT_WATCHDOG_S"}, Default: 30 * time.Second, Min: at(time.Duration(0))})

// Wait after a stop signal interrupts a one shot run, so the turn can wind down
var ShutdownSignalGrace = Register(&Duration{Meta: Meta{Key: "shutdown.signal_grace", Label: "Stop signal grace", Short: "Wait after a stop signal interrupts a one-shot run before it ends.", Help: "Zero ends it at once.", Group: GroupSystem, Order: 2230, Advanced: true, Apply: NewSession, Env: "ICEBATH_SIGTERM_GRACE"}, Default: 1500 * time.Millisecond, Min: at(time.Duration(0))})

// Windows console switched to UTF-8 text when icebath starts
var DisplayWindowsUTF8 = Register(&Bool{Meta: Meta{Key: "display.windows_utf8", Label: "Windows UTF-8", Short: "Switch the Windows console to UTF-8 text when icebath starts.", Help: "Off keeps the console's own code page.", Group: GroupSystem, Order: 2240, Advanced: true, Apply: NewSession, Env: "ICEBATH_DISABLE_WINDOWS_UTF8", EnvInvert: true}, Default: true})
