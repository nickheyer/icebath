package observability

import (
	"github.com/nickheyer/icebath/pkg/tools"
)

// Relay runtime identity keys the logical model call scope is stamped with
const (
	relayLogicalLLMScope      = "icebath.logical_llm_call"
	relayRuntimeSchemaKey     = "icebath.relay.schema_version"
	relayRuntimeSchemaVersion = "icebath.relay.runtime.v1"
	relayRuntimeInstanceKey   = "icebath.relay.runtime_instance"
)

const (
	SchemaKey               = "icebath.metrics.schema_version"
	CommitTicketKey         = "icebath.shared_metrics.commit_ticket"
	SchemaVersion           = "icebath.metrics.event.v4"
	ModelCallScope          = "icebath.model_call"
	ModelCallProfileModel   = "unknown"
	TaskScope               = "icebath.task_run"
	ToolCallScope           = "icebath.tool_call"
	ClientActiveMark        = "icebath.client.active"
	ToolApprovalMark        = "icebath.tool_approval"
	SkillLifecycleMark      = "icebath.skill.lifecycle"
	SkillLoadMark           = "icebath.skill.load"
	InstallSnapshotMark     = "icebath.install.snapshot"
	SessionMark             = "icebath.session"
	SetupCompletedMark      = "icebath.setup.completed"
	ModelTokensMark         = "icebath.model_tokens"
	CompressionMark         = "icebath.compression"
	ModelSwitchMark         = "icebath.model_switch"
	FallbackMark            = "icebath.fallback"
	SlashCommandMark        = "icebath.slash_command"
	ExtensionInstallMark    = "icebath.extension.install"
	StartupLatencyMark      = "icebath.startup.latency"
	SubscriberName          = "icebath.nemo_relay.shared_metrics"
	ClientActiveMetric      = "icebath.client.active"
	LegacyModelCallMetric   = "icebath.model_call.count"
	ModelRouteMetric        = "icebath.model_route.count"
	TaskStartedMetric       = "icebath.task_run.started"
	TaskFinishedMetric      = "icebath.task_run.finished"
	TaskDurationMetric      = "icebath.task_run.duration"
	ToolCallMetric          = "icebath.tool_call.count"
	ToolLatencyMetric       = "icebath.tool_call.latency"
	ToolApprovalMetric      = "icebath.tool_approval.count"
	SkillLifecycleMetric    = "icebath.skill.lifecycle.count"
	SkillLoadMetric         = "icebath.skill.load.count"
	ToolUsageMetric         = "icebath.tool.usage.count"
	InstallSnapshotMetric   = "icebath.install.snapshot"
	SessionMetric           = "icebath.session.count"
	MilestoneMetric         = "icebath.install.milestone"
	SetupCompletedMetric    = "icebath.setup.completed"
	ModelTokensMetric       = "icebath.model_tokens.sum"
	CompressionMetric       = "icebath.compression.count"
	ModelSwitchMetric       = "icebath.model_switch.count"
	FallbackMetric          = "icebath.fallback.count"
	SlashCommandMetric      = "icebath.slash_command.count"
	ExtensionInstallMetric  = "icebath.extension.install.count"
	MemoryOpMark            = "icebath.memory.op"
	CuratorRunMark          = "icebath.curator.run"
	DelegationRunMark       = "icebath.delegation.run"
	ExecutionBackendMark    = "icebath.execution_backend"
	MemoryOpMetric          = "icebath.memory.op.count"
	CuratorRunMetric        = "icebath.curator.run.count"
	DelegationRunMetric     = "icebath.delegation.run.count"
	ExecutionBackendMetric  = "icebath.execution_backend.count"
	ModelToolQualityMark    = "icebath.model_tool_quality"
	ModelFrictionMark       = "icebath.model_friction"
	ContextPeakMark         = "icebath.context_peak"
	ModelToolQualityMetric  = "icebath.model_tool_quality.count"
	ModelFrictionMetric     = "icebath.model_friction.count"
	ContextPeakMetric       = "icebath.context_peak.count"
	StartupLatencyMetric    = "icebath.startup.latency"
	EngagementTurnMark      = "icebath.engagement.turn"
	EngagementDayMetric     = "icebath.engagement.day.count"
	EngagementSurfaceMetric = "icebath.engagement.surface_day.count"
	ModelSwitchAfterMark    = "icebath.model_switch_after"
	ModelSwitchAfterMetric  = "icebath.model_switch_after.count"

	ModelIdentifierMaxLength    = 256
	ProviderIdentifierMaxLength = 64
)

const (
	metricIdentifierCharacters      = "abcdefghijklmnopqrstuvwxyz0123456789._:/@+-"
	metricIdentifierStartCharacters = "abcdefghijklmnopqrstuvwxyz0123456789"
)

var (
	ExecutionSurfaces = Set("acp", "api", "batch", "cli", "desktop", "gateway", "python", "scheduled_task", "tui",
		"other", "unknown")
	TaskOutcomes   = Set("cancelled", "failed", "success", "timed_out", "unknown")
	TaskEndReasons = Set("approval_denied", "completed", "failed", "guardrail_blocked", "iteration_limit",
		"system_aborted", "timed_out", "unknown", "user_cancelled")
	TaskTerminations = Set("none", "system_aborted", "timed_out", "unknown", "user_cancelled")
	TaskEntrypoints  = Set("api", "background", "batch", "delegated", "gateway_message", "interactive", "one_shot",
		"other", "python", "scheduled_task", "unknown")
	DurationBuckets = Set("1s_to_5s", "2m_to_10m", "30s_to_2m", "5s_to_30s", "gte_10m", "lt_1s")
	CountBuckets    = Set("0", "1", "2", "3_to_5", "6_to_10", "gte_11")
	ToolCategories  = Set("browser", "code_execution", "communication", "computer_use", "connector", "delegation",
		"file", "home_automation", "mcp", "media", "memory", "other", "planning", "project", "scheduler",
		"skill", "terminal", "unknown", "web")
	ToolOutcomes             = Set("blocked", "cancelled", "failed", "success", "timed_out", "unknown")
	ToolApprovalOutcomes     = Set("approved", "cancelled", "denied", "not_required", "timed_out", "unknown")
	ToolApprovalAttributions = Set("tool_call", "unattributed")
	ToolLatencyBuckets       = Set("100ms_to_250ms", "10s_to_30s", "1s_to_2s", "250ms_to_500ms", "2s_to_5s",
		"500ms_to_1s", "5s_to_10s", "gte_30s", "lt_100ms", "unknown")
	ToolRetryBuckets      = CountBuckets.Union(Set("unknown"))
	SkillLifecycleActions = Set("archived", "created", "edited", "installed", "patched", "restored", "stale")
	SkillProvenances      = Set("agent_created", "external", "installed", "local", "unknown")
	SkillReuseStates      = Set("first_use", "reused")
	SkillPostPatchStates  = Set("no_new_patch", "not_applicable", "reused_after_patch")
	ClientOSFamilies      = Set("linux", "macos", "unknown", "windows")
	ClientArchitectures   = Set("arm", "arm64", "unknown", "x86", "x86_64")
	ClientInstallMethods  = Set("apt", "docker", "git", "home-manager", "homebrew", "microsoft-store", "nixos", "pip",
		"unknown")
	ClientResourceKeys = Set("architecture", "icebath_version", "install_method", "os_family")

	ModelCallRoles = Set("auxiliary", "primary")
	ModelOutcomes  = Set("cancelled", "failed", "success")
	// The classifier's closed FailoverReason enum plus none, read through the provider hook
	ModelErrorClasses lazySet = func() ValueSet { return failoverReasonValues().Union(Set("none")) }
	// Task failures add local classes to every model error class
	TaskFailureClasses lazySet = func() ValueSet {
		return ModelErrorClasses().Union(Set("context_compression", "empty_response", "exception", "local_error",
			"other", "persistence", "repeated_errors", "restart_limit", "session_busy", "shutdown"))
	}
	coreGatewayPlatforms = platformRegistryNames().Minus("api_server", "cli", "cron").Union(Set("none", "plugin", "relay"))
	BridgeToolNames      = Set("tool_call", "tool_describe", "tool_search")
	RetiredToolNames     = Set("setup_choose", "start_chat",
		"browser_back", "browser_click", "browser_console", "browser_get_images", "browser_navigate",
		"browser_press", "browser_scroll", "browser_snapshot", "browser_type", "browser_vision",
		"browser_vault_enter_code", "browser_vault_fill", "browser_vault_list", "browser_vault_save_login",
		"browser_vault_unlock",
		"spotify_albums", "spotify_devices", "spotify_library", "spotify_playback", "spotify_playlists",
		"spotify_queue", "spotify_search")
	RetiredToolsetNames = Set("setup", "spotify", "start_chat")
	liveToolNames       = setOfKeys(tools.BuiltinToolNames).Union(BridgeToolNames, Set("connector", "mcp", "plugin", "unknown"))
	ToolNames           = liveToolNames.Union(RetiredToolNames)
	ToolErrorClasses    = Set("alias", "blocked", "connection_required", "connector_backend", "contract_violation",
		"exception", "interrupted", "invalid_arguments", "none", "not_allowed", "timeout", "tool_error", "unknown")
	SizeBuckets = Set("0", "1", "2", "3_to_5", "6_to_10", "11_to_25", "26_to_100", "101_to_250", "gte_251")

	MemoryProviders = &CatalogValues{loaders: []string{"plugin_catalog_names"},
		extraFn: func() ValueSet { return bundledMemoryProviders().Union(Set("builtin", "plugin")) }}

	SessionDurationBuckets = Set("lt_1m", "1m_to_5m", "5m_to_30m", "30m_to_2h", "2h_to_8h", "gte_8h")
	InstallAgeBuckets      = Set("lt_1h", "1h_to_1d", "1d_to_7d", "7d_to_30d", "30d_to_90d", "gte_90d", "unknown")
	Milestones             = Set("setup_completed", "first_task_started", "first_task_success", "first_tool_success",
		"first_gateway_message", "first_scheduled_task", "first_delegation", "first_skill_created",
		"first_skill_reused", "first_mcp_tool_success", "first_long_session")
	SetupSurfaces = Set("cli", "desktop", "other")
	TokenTypes    = Set("input", "output", "cache_read", "cache_write", "reasoning")
	TTFTBuckets   = Set("lt_500ms", "500ms_to_1s", "1s_to_2s", "2s_to_5s", "5s_to_15s", "gte_15s", "not_streamed",
		"unknown")
	CompressionTriggers    = Set("auto", "manual", "other", "overflow")
	CompressionOutcomes    = Set("failed", "skipped", "success")
	CompressionSkipClasses = Set("attempt_superseded", "empty_post_handoff_window", "explicit_interrupt",
		"insufficient_messages", "lock_contended", "no_compressible_window", "snapshot_stale")
	CompressionFailureClasses = CompressionSkipClasses.Union(Set("aux_model_fallback", "commit_fence_cancelled",
		"empty_transcript", "exception", "feasibility_skip", "no_progress", "none",
		"other", "pool_saturated", "rollback", "session_split_failed", "stall_deterministic_fallback",
		"stall_interrupted", "summary_auth_failure", "summary_empty_content_failure", "summary_generation_aborted",
		"summary_generation_failed", "summary_network_failure", "summary_overload_degraded",
		"summary_overload_failure", "summary_truncated_failure", "unknown", "would_grow"))
	ContextFillBuckets = Set("lt_50", "50_to_75", "75_to_90", "90_to_100", "gte_100", "unknown")
	ExtensionKinds     = Set("mcp_server", "plugin", "skill")
	ExtensionSources   = Set("bundled", "catalog", "hub", "local", "other", "url")
	ExtensionOutcomes  = Set("failed", "success")
	TerminalBackends   = Set("daytona", "docker", "local", "managed_modal", "modal", "other", "singularity", "ssh",
		"vercel_sandbox")
	AuxTasks             = &CatalogValues{loaders: []string{"aux_task_names"}, extra: Set("none", "other")}
	SlashCommands        = &CatalogValues{loaders: []string{"slash_command_names"}, extra: Set("plugin", "skill", "unknown")}
	SkillNames           = &CatalogValues{loaders: []string{"bundled_skill_names"}, extra: Set("custom")}
	ExtensionNames       = &CatalogValues{loaders: []string{"bundled_skill_names", "mcp_catalog_names", "plugin_catalog_names"}, extra: Set("custom")}
	DisplayLanguages     = &CatalogValues{loaders: []string{"display_languages"}, extra: Set("other")}
	ToolCallIssues       = Set("empty_arguments", "invalid_json", "none", "repaired", "schema_mismatch", "unknown_tool")
	FrictionSignals      = Set("interrupt", "quick_abandon", "retry", "switch_away", "undo")
	ContextWindowBuckets = Set("lt_32k", "32k_to_128k", "128k_to_256k", "256k_to_1m", "gte_1m", "unknown")
	LimitHitValues       = Set("no", "yes")

	MemoryOps              = Set("add", "other", "read", "remove", "replace", "search")
	MemoryOpOutcomes       = Set("failed", "rejected", "success")
	MemoryOpOrigins        = Set("background_review", "foreground")
	MemoryOpFailureClasses = Set("ambiguous", "disabled", "drift", "exception", "gate_refused", "invalid_args",
		"missing_content", "missing_old_text", "no_match", "none", "other", "over_budget", "provider_error",
		"read_failed", "retry_cap", "scan_blocked", "staged", "stale_entry", "unknown", "would_empty")
	CuratorOutcomes    = Set("failed", "skipped", "success")
	CuratorTriggers    = Set("manual", "scheduled")
	DelegationOutcomes = Set("cancelled", "failed", "partial", "success")
	DelegationDepths   = Set("1", "2", "3", "gte_4")
	DelegationModes    = Set("background", "foreground")
	ExecutionKinds     = Set("browser", "code", "terminal")
	BrowserBackends    = Set("browser-use", "browserbase", "camofox", "cdp", "extension", "firecrawl", "lightpanda",
		"local", "other")
	CodeBackends            = Set("local", "other", "remote")
	ExecutionBackendsByKind = map[string]ValueSet{"browser": BrowserBackends, "code": CodeBackends, "terminal": TerminalBackends}
	ExecutionBackends       = BrowserBackends.Union(CodeBackends, TerminalBackends)
	ExecutionOutcomes       = Set("failed", "success")
)

const (
	PlatformHealthMark     = "icebath.platform.health"
	PlatformHealthMetric   = PlatformHealthMark
	PlatformDeliveryMark   = "icebath.platform.delivery"
	PlatformDeliveryMetric = PlatformDeliveryMark
	ReplyLatencyMark       = "icebath.gateway.reply_latency"
	ReplyLatencyMetric     = ReplyLatencyMark
	CronRunMark            = "icebath.cron.run"
	CronRunMetric          = CronRunMark
)

var (
	platformCatalogs = []string{"bundled_platform_names", "catalog_platform_names"}
	GatewayPlatforms = &CatalogValues{loaders: platformCatalogs, extra: coreGatewayPlatforms}
	// Adapter platforms add the gateway's own Platform enum members to the catalog platforms
	AdapterPlatforms = &CatalogValues{loaders: platformCatalogs, extra: coreGatewayPlatforms.Union(Set("api_server")),
		adapters: true}
	PlatformHealthEvents   = Set("connect_ok", "connect_failed", "reconnect", "disconnect")
	PlatformErrorClasses   = Set("none", "auth", "network", "rate_limited", "config", "other")
	DeliveryOutcomes       = Set("sent", "failed")
	DeliveryFailureClasses = Set("none", "rate_limited", "too_long", "auth", "network", "forbidden", "other")
	ReplyLatencyBuckets    = Set("lt_2s", "2s_to_5s", "5s_to_15s", "15s_to_60s", "gte_60s")
	CronRunOutcomes        = Set("success", "failed", "missed", "skipped", "cancelled")
	CronDeliveryKinds      = Set("local", "platform", "webhook", "none", "other")

	StartupSurfaces             = Set("cli", "desktop_attach", "gateway_boot", "serve_boot", "tui")
	StartupLatencyBuckets       = Set("lt_500ms", "500ms_to_1s", "1s_to_2s", "2s_to_5s", "5s_to_10s", "gte_10s")
	ReleaseChannels             = Set("dev", "main", "stable", "unknown")
	VersionAgeBuckets           = Set("lt_7d", "7d_to_30d", "30d_to_90d", "gte_90d", "unknown")
	BehindBuckets               = CountBuckets.Union(Set("unknown"))
	RAMBuckets                  = Set("lt_8g", "8g_to_16g", "16g_to_32g", "32g_to_64g", "64g_to_128g", "gte_128g", "unknown")
	GPUClasses                  = Set("amd", "apple_silicon", "intel", "none", "nvidia", "unknown")
	YesNo                       = Set("no", "yes")
	installV4SnapshotDimensions = dims{
		"behind_bucket": BehindBuckets, "gpu_class": GPUClasses, "local_model_provider_used": YesNo,
		"ram_bucket": RAMBuckets, "release_channel": ReleaseChannels, "version_age_bucket": VersionAgeBuckets,
	}

	EngagementSurfaces       = Set("acp", "cli", "cron", "desktop", "gateway", "tui")
	ActiveMinutesBuckets     = Set("0", "lt_5m", "5m_to_30m", "30m_to_2h", "2h_to_6h", "gte_6h")
	SurfacesUsedBuckets      = Set("0", "1", "2", "3", "gte_4")
	TurnsBeforeSwitchBuckets = Set("1", "2_to_3", "4_to_10", "11_to_30", "gte_31")
	LongSizeBuckets          = SizeBuckets.Union(Set("251_to_1000", "gte_1001"))
	longSizeThresholds       = []threshold{{1, "0"}, {2, "1"}, {3, "2"}, {6, "3_to_5"}, {11, "6_to_10"}, {26, "11_to_25"}, {101, "26_to_100"}, {251, "101_to_250"}, {1001, "251_to_1000"}}
)

// Bucket of a non negative count on the long size scale that tops out at gte_1001
func LongSizeBucket(count int) string {
	return bucket(clampCount(count), longSizeThresholds, "gte_1001")
}

const (
	UpdateRunMark         = "icebath.update.run"
	UpdateRunMetric       = UpdateRunMark
	UpdateStageMark       = "icebath.update.stage"
	UpdateStageMetric     = UpdateStageMark
	ProcessExitMark       = "icebath.process.exit"
	ProcessExitMetric     = ProcessExitMark
	dayMS                 = 86_400_000
	InstallRunMark        = "icebath.install.run"
	InstallRunMetric      = InstallRunMark
	ToolUnavailableMark   = "icebath.tool_unavailable.count"
	ToolUnavailableMetric = ToolUnavailableMark
	ProviderSetupMark     = "icebath.provider_setup.count"
	ProviderSetupMetric   = ProviderSetupMark
	FeatureAdoptionMetric = "icebath.feature_adoption.count"
	FeatureUsedMark       = "icebath.feature.used"
	FeatureDisabledMark   = "icebath.feature_disabled.count"
	FeatureDisabledMetric = FeatureDisabledMark

	FeatureDisabledNameMaxLength = 64
)

var (
	UpdateKinds              = Set("cli", "desktop")
	UpdateOutcomes           = Set("failed", "noop", "refused", "success")
	UpdateRunOutcomes        = UpdateOutcomes.Union(Set("partial"))
	UpdateStageOrder         = []string{"plan", "snapshot", "apply", "deps", "build", "restart", "verify"}
	UpdateStages             = Set(UpdateStageOrder...)
	DesktopUpdateStages      = Set("apply", "download", "restart", "verify")
	UpdateFailedStages       = UpdateStages.Union(DesktopUpdateStages, Set("none", "other"))
	UpdateStageOutcomes      = Set("failed", "skipped", "success")
	UpdateDurationBuckets    = Set("lt_30s", "30s_to_2m", "2m_to_5m", "5m_to_15m", "gte_15m")
	UpdateApplyModes         = Set("external", "git", "package", "unknown", "zip")
	ProcessKinds             = Set("cli", "cron", "gateway", "other", "serve", "tui")
	ProcessExitKinds         = Set("clean", "crash", "killed", "watchdog")
	CrashClasses             = Set("import_error", "memory_error", "none", "os_error", "other", "runtime_error")
	updateDurationThresholds = []threshold{{30_000, "lt_30s"}, {120_000, "30s_to_2m"}, {300_000, "2m_to_5m"}, {900_000, "5m_to_15m"}}
	versionAgeThresholds     = []threshold{{7 * dayMS, "lt_7d"}, {30 * dayMS, "7d_to_30d"}, {90 * dayMS, "30d_to_90d"}}

	ProviderSetupSurfaces       = Set("cli_model", "cli_setup", "dashboard", "desktop", "tui")
	ProviderSetupEvents         = Set("abandoned", "completed", "failed", "started")
	ProviderSetupFailureClasses = Set("auth", "cancelled", "network", "no_models", "none", "other")
	Features                    = Set("browser", "bot_mode", "cron", "curator", "delegation", "desktop", "gateway_platform", "kanban",
		"mcp", "memory", "plugins", "projects", "skills_created", "tui", "voice")
	DaysSinceInstallBuckets = Set("same_day", "1d_to_7d", "7d_to_30d", "30d_to_90d", "gte_90d", "unknown")
	FeatureDisabledKinds    = Set("compression", "curator", "memory", "platform", "plugin", "setting", "skill", "toolset")
	FeatureDisabledSurfaces = Set("cli_config", "cli_slash", "cli_tools", "dashboard", "desktop", "tui")
	FeatureDisabledEvents   = Set("disabled", "re_enabled")

	ExtensionCommonFailureClasses = Set("exception", "filesystem_error", "network", "none", "other", "permission")
	ExtensionSkillFailureClasses  = ExtensionCommonFailureClasses.Union(Set("ambiguous", "auth_rejected",
		"fetch_failed", "invalid_bundle", "invalid_name", "not_found", "rate_limited", "scan_blocked", "stale_index"))
	ExtensionPluginFailureClasses = ExtensionCommonFailureClasses.Union(Set("already_installed", "clone_failed",
		"deps_declined", "deps_failed", "git_missing", "incompatible", "invalid_source", "manifest_invalid",
		"non_interactive", "removed_from_catalog", "scan_blocked"))
	ExtensionMCPFailureClasses = ExtensionCommonFailureClasses.Union(Set("auth_required", "bootstrap_failed",
		"clone_failed", "config_invalid", "config_rejected", "connect_failed", "git_missing", "missing_credentials",
		"server_start_failed"))
	ExtensionKindFailureClasses = map[string]ValueSet{
		"mcp_server": ExtensionMCPFailureClasses, "plugin": ExtensionPluginFailureClasses,
		"skill": ExtensionSkillFailureClasses,
	}
	ExtensionFailureClasses = ExtensionSkillFailureClasses.Union(ExtensionPluginFailureClasses, ExtensionMCPFailureClasses)
	ExtensionRegistryIDs    = Set("browse-sh", "clawhub", "github", "hermes-index", "lobehub", "official", "skills-sh",
		"url", "well-known")
	ExtensionRegistries = ExtensionRegistryIDs.Union(Set("none", "other", "unresolved"))
	UpdateStopClasses   = Set("branch_missing", "branch_unsupported", "channel_unresolved", "checkout_move_failed",
		"commit_point_refused", "detached_head", "disk_full", "downgrade_refused", "download_failed", "fetch_failed",
		"gateway_pause_failed", "git_in_progress", "git_index_locked", "git_timeout", "head_moved",
		"local_changes_blocked", "lock_held", "managed_install", "merge_conflict", "not_git_checkout",
		"old_version_handoff", "parked_branch_blocked", "permission_denied", "stash_restore_rejected",
		"syntax_rollback", "target_syntax_error", "target_unresolved", "unexpected_branch", "venv_foreign_owner",
		"zip_failed")
	UpdateFailureClasses = Set("aborted_before_apply", "build_failed", "deps_failed", "exception", "fleet_stale",
		"fleet_unverified", "git_failed", "interrupted", "lock_held", "managed_install", "none", "other", "os_error",
		"restart_failed", "subprocess_failed", "unknown", "disk_full", "local_changes_parked",
		"permission_denied").Union(UpdateStopClasses)
	InstallRunInstallers     = Set("install_ps1", "install_sh", "other")
	InstallRunOutcomes       = Set("failed", "success")
	InstallRunStages         = Set("complete", "config", "gateway", "prerequisites", "products", "python_deps", "repository", "setup", "venv")
	InstallRunFailedStages   = InstallRunStages.Union(Set("none", "other"))
	InstallRunFailureClasses = Set("commit_not_on_branch", "curl_missing", "deps_install_failed", "dir_not_checkout",
		"download_digest_mismatch", "download_failed", "filesystem_error", "gateway_failed", "git_checkout_failed",
		"git_clone_failed", "git_extract_failed", "git_fetch_failed", "git_missing", "git_reset_failed", "interrupted",
		"libstdcxx_missing", "local_changes_blocked", "none", "other", "products_build_failed",
		"python_install_failed", "setup_failed", "unsupported_platform", "uv_unusable")
)

// Bucket of an update or update stage wall time, non numbers counting as instant
func UpdateDurationBucket(durationMS any) string {
	value, _ := nonNegativeNumber(durationMS)
	return bucket(value, updateDurationThresholds, "gte_15m")
}

// Bucket of the age of the version an update started from, unknown when not measurable
func VersionAgeBucket(ageMS any) string {
	value, ok := nonNegativeNumber(ageMS)
	if !ok {
		return "unknown"
	}
	return bucket(value, versionAgeThresholds, "gte_90d")
}

const (
	FileEditMark               = "icebath.file_edit"
	LoopGuardMark              = "icebath.loop_guard"
	ToolRecoveryMark           = "icebath.tool_recovery"
	TerminalOutcomeMark        = "icebath.terminal.outcome"
	ModelReplyIssueMark        = "icebath.model_reply_issue"
	FileEditMetric             = "icebath.file_edit.count"
	LoopGuardMetric            = "icebath.loop_guard.count"
	ToolRecoveryMetric         = "icebath.tool_recovery.count"
	TerminalOutcomeMetric      = "icebath.terminal.outcome.count"
	ModelReplyIssueMetric      = "icebath.model_reply_issue.count"
	ToolSearchMark             = "icebath.tool_search.count"
	ToolSearchMetric           = ToolSearchMark
	ToolBatchMark              = "icebath.tool_batch.count"
	ToolBatchMetric            = ToolBatchMark
	ConnectorChainMark         = "icebath.connector_chain.count"
	ConnectorChainMetric       = ConnectorChainMark
	TaskCostMark               = "icebath.task_cost.count"
	TaskCostMetric             = TaskCostMark
	WastedTokensMark           = "icebath.wasted_tokens.count"
	WastedTokensMetric         = WastedTokensMark
	ToolOutputTruncationMark   = "icebath.tool_output_truncation.count"
	ToolOutputTruncationMetric = ToolOutputTruncationMark
	ToolOverheadMark           = "icebath.tool_overhead.count"
	ToolOverheadMetric         = ToolOverheadMark
	ToolEnabledUnusedMark      = "icebath.tool_enabled_unused.count"
	ToolEnabledUnusedMetric    = ToolEnabledUnusedMark
	CacheBreakMark             = "icebath.cache_break.count"
	CacheBreakMetric           = CacheBreakMark
	DesktopFeatureUseMark      = "icebath.desktop.feature_use"
	DesktopFeatureUseMetric    = DesktopFeatureUseMark
	DesktopFrictionMark        = "icebath.desktop.friction"
	DesktopFrictionMetric      = DesktopFrictionMark
	DesktopOnboardingMark      = "icebath.desktop.onboarding"
	DesktopOnboardingMetric    = DesktopOnboardingMark
	DesktopModeUseMark         = "icebath.desktop.mode_use"
	DesktopModeUseMetric       = DesktopModeUseMark
	DesktopActionUseMark       = "icebath.desktop.action_use"
	DesktopActionUseMetric     = DesktopActionUseMark
	DesktopDislikeMark         = "icebath.desktop.dislike"
	DesktopDislikeMetric       = DesktopDislikeMark

	DesktopSettingKeyMaxLength = 96
)

var (
	FileEditTools    = Set("patch", "write_file")
	FileEditModes    = Set("replace", "v4a", "whole_file")
	FileEditOutcomes = Set("already_applied", "ambiguous", "applied", "failed", "no_match")
	// Every fuzzy matcher strategy name plus none
	FileEditStrategies = Set(tools.FuzzyStrategyNames()...).Union(Set("none"))
	LoopGuardSignals   = Set("iteration_cap", "loop_detected", "repeated_tool_call")
	LoopGuardDetectors = Set("exact_failure", "idempotent_no_progress", "identical_call_streak", "identical_cycle",
		"iteration_budget", "same_tool_failure", "subagent_cap", "web_search_cap")
	ToolRecoveryOutcomes  = Set("error", "gave_up", "no_tool_call", "success")
	ToolRecoveryNextTools = Set("different", "none", "same")
	ToolSearchScopes      = Set("both", "connector", "local")
	ToolSearchOutcomes    = Set("connector_unavailable", "error", "hits", "no_hits")
	ConnectorShares       = Set("all", "none", "some")
	TerminalCommandKinds  = Set("build", "container", "file_ops", "git", "network", "node", "other", "package_manager",
		"python", "shell", "shell_builtin", "test_runner")
	TerminalOutcomes       = Set("killed", "nonzero", "ok", "timeout")
	ModelReplyIssues       = Set("empty", "none", "reasoning_only", "refusal", "truncated_length")
	TurnTokenBuckets       = Set("lt_2k", "2k_to_10k", "10k_to_50k", "50k_to_200k", "200k_to_1m", "gte_1m", "unknown")
	TaskCostOutcomes       = Set("completed", "failed", "interrupted")
	TurnActivityBuckets    = Set("0", "1", "2", "3_to_5", "6_to_10", "11_to_25", "26_to_50", "51_to_100", "gte_101")
	WasteReasons           = Set("interrupt", "retry", "undo")
	ToolOutputSizeBuckets  = Set("lt_1k", "1k_to_10k", "10k_to_50k", "50k_to_100k", "100k_to_500k", "gte_500k", "unknown")
	ToolSchemaTokenBuckets = Set("0", "lt_2k", "2k_to_5k", "5k_to_10k", "10k_to_20k", "20k_to_40k", "gte_40k")
	ToolsetNames           = setOfKeys(tools.BuiltinToolsetNames).Union(RetiredToolsetNames, Set("custom"))
	CacheBreakCauses       = Set("cache_expired", "compression", "model_switch", "provider_reported_miss",
		"system_prompt_rebuild", "toolset_change")

	DesktopSettingsAreas = Set("settings_about", "settings_billing", "settings_config_advanced",
		"settings_config_appearance", "settings_config_browser", "settings_config_chat", "settings_config_memory",
		"settings_config_model", "settings_config_safety", "settings_config_voice", "settings_config_workspace",
		"settings_gateway", "settings_keybinds", "settings_keys", "settings_notifications", "settings_other",
		"settings_providers", "settings_sessions", "settings_vault")
	DesktopFeatureAreas = DesktopSettingsAreas.Union(Set("browser_pane", "file_pane", "review_pane", "terminal_pane",
		"command_palette", "find_in_page", "model_picker", "session_picker", "session_switcher",
		"bot_mode", "projects", "session_search", "skins", "voice_conversation", "voice_dictation",
		"agents", "artifacts", "capabilities", "command_center", "cron", "extension_page", "kanban", "messaging",
		"profiles", "session_import", "starmap", "webhooks", "other"))
	DesktopNoticeIDs = Set("artifacts_partial_load", "background_queue_stuck", "backend_skew", "billing_banner",
		"billing_block", "build_discontinued", "client_behind", "composer_queue_stuck", "credits",
		"free_tier_notice", "gateway_error", "gui_skew", "install_method", "mcp_health", "model_warning",
		"onboarding_handoff", "restored_draft", "runtime_not_ready", "session_compress", "terminal_backend", "tip",
		"update_available", "voice_live_unavailable", "voice_stop_hint", "other")
	DesktopErrorToastCategories = Set("api_key_missing", "api_key_rejected", "disk_full", "gateway_auth_failed",
		"method_not_allowed", "microphone_permission", "pool_slot_timeout", "restart_required", "rpc_out_of_sync",
		"storage_failure", "timeout", "unclassified", "other")
	DesktopSlowFrameBuckets = Set("100ms_to_250ms", "250ms_to_1s", "1s_to_5s", "gte_5s")
	DesktopFrictionDetails  = map[string]ValueSet{
		"backend_disconnect": Set("backend_exit", "network", "timeout", "other"),
		"error_toast":        DesktopErrorToastCategories,
		"notice_dismissed":   DesktopNoticeIDs,
		"renderer_crash":     Set("crash", "killed", "oom", "other"),
		"slow_frame":         DesktopSlowFrameBuckets,
	}
	DesktopFrictionKinds        = keysOf(DesktopFrictionDetails)
	DesktopFrictionDetailValues = unionOf(DesktopFrictionDetails)
	DesktopOnboardingSteps      = Set("choose_later", "consent", "first_message", "free_tier_ready", "guide",
		"guide_skip", "model_pick", "provider_api_key", "provider_local", "provider_oauth", "provider_setup", "sign_in")
	DesktopOnboardingEvents     = Set("abandoned", "completed", "reached")
	DesktopModes                = Set("bots", "sessions")
	DesktopActiveMinutesBuckets = Set("0", "lt_5m", "5m_to_30m", "30m_to_2h", "2h_to_6h", "gte_6h")
	desktopActiveThresholds     = []threshold{{300_000, "lt_5m"}, {1_800_000, "5m_to_30m"}, {7_200_000, "30m_to_2h"}, {21_600_000, "2h_to_6h"}}
	DesktopKeybindActionIDs     = Set("appearance.toggleMode", "composer.cancel", "composer.dictate", "composer.focus",
		"composer.help", "composer.history", "composer.mention", "composer.modelPicker", "composer.newline",
		"composer.queue", "composer.reasoningDown", "composer.reasoningUp", "composer.send", "composer.sendQueued",
		"composer.slash", "composer.steer", "composer.voice", "conversation.scrollPageDown",
		"conversation.scrollPageUp", "hud.snapToPointer", "keybinds.openPanel", "nav.agents", "nav.artifacts",
		"nav.capabilities", "nav.commandCenter", "nav.commandPalette", "nav.cron", "nav.messaging", "nav.profiles",
		"nav.settings", "profile.create", "profile.default", "profile.next", "profile.prev", "profile.toggleAll",
		"session.archive", "session.focusSearch", "session.new", "session.newTab", "session.newWindow",
		"session.next", "session.prev", "session.togglePin", "view.closeTab", "view.closeTerminal",
		"view.cycleSidebarGrouping", "view.findInPage", "view.findNext", "view.findPrevious", "view.flipPanes",
		"view.newTerminal", "view.nextTerminal", "view.prevTerminal", "view.reopenTab", "view.selectionToComposer",
		"view.showBrowser", "view.showFiles", "view.showTerminal", "view.terminalCopy", "view.terminalPaste",
		"view.toggleHud", "view.toggleProfileRail", "view.toggleReview", "view.toggleRightSidebar",
		"view.toggleSidebar", "view.toggleSimpleMode", "view.toggleStatusbar", "view.toggleTabStrip",
		"workspace.newWorktree", "workspace.openFolder")
	DesktopButtonActionIDs = Set("composer.attach", "message.copy", "message.retry")
	DesktopActionIDs       = DesktopKeybindActionIDs.Union(DesktopButtonActionIDs, Set("other"))
	DesktopActionVias      = Set("click", "menu", "palette", "shortcut")
	DesktopFlowIDs         = Set("command_palette", "free_tier_sign_in", "keybind_capture", "model_picker",
		"project_create", "provider_oauth", "session_picker", "session_switcher", "other")
	DesktopFeatureToggles = Set("backdrop", "bot_activity_toasts", "composer_popout_gestures", "intro_splash",
		"native_notifications", "notification_kind", "reactions", "thread_timeline", "tips", "tours", "vibe_hearts",
		"other")
	DesktopUndoTargets    = Set("closed_tab", "restored_draft", "other")
	DesktopDislikeTargets = map[string]ValueSet{
		"cancelled":           DesktopFlowIDs,
		"feature_disabled":    DesktopFeatureToggles,
		"quick_close":         DesktopFeatureAreas,
		"rage_click":          DesktopActionIDs,
		"setting_off_default": Set("setting"),
		"undo":                DesktopUndoTargets,
	}
	DesktopDislikeSignals      = keysOf(DesktopDislikeTargets)
	DesktopDislikeTargetValues = unionOf(DesktopDislikeTargets)
	DesktopDislikeDirections   = Set("away_from_default", "none", "to_default")
)

// Union of every set in a map
func unionOf(m map[string]ValueSet) ValueSet {
	out := ValueSet{}
	for _, s := range m {
		out = out.Union(s)
	}
	return out
}

// Bucket of a day's active time in one Desktop mode, nothing measurable reading 0
func DesktopActiveMinutesBucket(activeMS any) string {
	value, _ := nonNegativeNumber(activeMS)
	if value <= 0 {
		return "0"
	}
	return bucket(value, desktopActiveThresholds, "gte_6h")
}
