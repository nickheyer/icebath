package home

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Location below a home directory, written with forward slashes
type Entry string

// Path of the location below home
func (e Entry) In(home string) string { return filepath.Join(home, filepath.FromSlash(string(e))) }

// Location below the root that every profile of one install shares
type RootEntry string

// Path of the location below root
func (e RootEntry) In(root string) string { return filepath.Join(root, filepath.FromSlash(string(e))) }

// Files and directories that identify a home, its configuration and its persona
const (
	Config               Entry = "config.yaml"
	EnvFile              Entry = ".env"
	EnvLock              Entry = ".env.lock"
	OpEnvFile            Entry = ".op.env"
	AuthFile             Entry = "auth.json"
	AuthLock             Entry = "auth.lock"
	AuthCorrupt          Entry = "auth.json.corrupt"
	CredentialsFile      Entry = "credentials.json"
	Soul                 Entry = "SOUL.md"
	ProfileManifest      Entry = "profile.yaml"
	DistributionManifest Entry = "distribution.yaml"
	ManagedMarker        Entry = ".managed"
	ContainerMode        Entry = ".container-mode"
	InstallMethod        Entry = ".install_method"
	SystemPromptFile     Entry = "system_prompt.md"
	AgentsFile           Entry = "AGENTS.md"
	ClaudeFile           Entry = "CLAUDE.md"
	CursorRules          Entry = ".cursorrules"
	TodoFile             Entry = "todo.json"
	DesktopAppearance    Entry = "desktop.json"
	Knowledge            Entry = "knowledge"
	Preferences          Entry = "preferences"
	HomeMemoryFile       Entry = "MEMORY.md"
	HomeUserFile         Entry = "USER.md"
	HonchoConfig         Entry = "honcho.json"
	ImportSync           Entry = "import-sync.json"
	Assets               Entry = "assets"
	Plans                Entry = "plans"
	Workspace            Entry = "workspace"
	SubprocessHomeDir    Entry = "home"
)

// Encrypted autofill vault and the secret source caches kept at the top of a home
const (
	Vault                   Entry = "vault"
	VaultData               Entry = "vault/vault.json.enc"
	VaultKey                Entry = "vault/vault.key"
	VaultKeyRef             Entry = "vault/vault.keyref"
	VaultLock               Entry = "vault/.vault.lock"
	BitwardenCache          Entry = "bws_cache.json"
	BitwardenEncryptedCache Entry = "bws_cache.enc.json"
	OnePasswordCache        Entry = "op_cache.json"
	AnthropicOAuth          Entry = ".anthropic_oauth.json"
	GoogleToken             Entry = "google_token.json"
	RateLimits              Entry = "rate_limits"
	PortalRateLimit         Entry = "rate_limits/portal.json"
	PortalAnonymousLimit    Entry = "rate_limits/portal-anonymous.json"
)

// Databases kept at the top of a home
const (
	StateDB                Entry = "state.db"
	ProjectsDB             Entry = "projects.db"
	ResponseStoreDB        Entry = "response_store.db"
	RunsIdempotencyDB      Entry = "runs_idempotency.db"
	VerificationEvidenceDB Entry = "verification_evidence.db"
	RetainDBQueue          Entry = "retaindb_queue.db"
)

// Gateway, service and process runtime state
const (
	ProcessesFile             Entry = "processes.json"
	ProcessesLock             Entry = "processes.json.lock"
	GatewayPID                Entry = "gateway.pid"
	GatewayLock               Entry = "gateway.lock"
	GatewaySocket             Entry = "gateway.sock"
	GatewaySocketPointer      Entry = "gateway.sock.path"
	GatewayConfig             Entry = "gateway.json"
	GatewayParked             Entry = "gateway.parked"
	GatewayState              Entry = "gateway_state.json"
	GatewayPlannedStop        Entry = ".gateway-planned-stop.json"
	GatewayTakeover           Entry = ".gateway-takeover.json"
	GatewayStartsLog          Entry = "gateway-starts.log"
	GatewayLaunchdUnsupported Entry = ".gateway-launchd-unsupported"
	GatewayVoiceMode          Entry = "gateway_voice_mode.json"
	GatewayMigration          Entry = "gateway_migration.json"
	GatewayDir                Entry = "gateway"
	GatewayDeadTargets        Entry = "gateway/dead_targets.json"
	GatewayRestartLoop        Entry = "gateway/restart_loop.json"
	DiscordCommandSyncState   Entry = "gateway/discord_command_sync_state.json"
	DiscordMessageRecovery    Entry = "gateway/discord_message_recovery.db"
	DiscordNonconversational  Entry = "gateway/discord_nonconversational_messages.json"
	GatewayService            Entry = "gateway-service"
	DrainRequest              Entry = ".drain_request.json"
	MultiplexOptOutRewritten  Entry = ".multiplex_opt_out_rewritten"
	ServeRestartPending       Entry = "serve_restart_pending"
	FleetRestartPending       Entry = "fleet_restart_pending"
	RestartNotify             Entry = ".restart_notify.json"
	RestartPending            Entry = ".restart_pending.json"
	RestartLastProcessed      Entry = ".restart_last_processed.json"
	CleanShutdown             Entry = ".clean_shutdown"
	EmergencyStop             Entry = "ESTOP"
	RoomLinkGrantSecret       Entry = ".room-link-grant-secret"
	RuntimeDir                Entry = "runtime"
	ActiveSessions            Entry = "runtime/active_sessions.json"
	ActiveSessionsLock        Entry = "runtime/active_sessions.lock"
	BotLiveDelivery           Entry = "runtime/bot_live_delivery"
	BotLiveDeliveryLock       Entry = "runtime/bot_live_delivery/.lock"
	BotLiveDeliverySequence   Entry = "runtime/bot_live_delivery/.sequence"
	StateDir                  Entry = "state"
	RichSentIndex             Entry = "state/rich_sent_index.json"
	GatewayHeartbeat          Entry = "state/gateway.heartbeat"
	DashboardComputeHost      Entry = "state/dashboard-compute-host.json"
	PendingDir                Entry = "pending"
	PendingMessages           Entry = "pending_messages"
	SpawnTrees                Entry = "spawn-trees"
	TerminalSessions          Entry = "terminal-sessions"
	TelephonyState            Entry = "telephony_state.json"
	WorkerOutboxes            Entry = "worker-outboxes"
	FileSyncLock              Entry = ".sync.lock"
	WebhookSubscriptions      Entry = "webhook_subscriptions.json"
	WebhookSubscriptionsLock  Entry = ".webhook_subscriptions.lock"
	DesktopState              Entry = "desktop"
	InterruptedTurns          Entry = "desktop/interrupted_turns.json"
)

// Update, backup, prune advice and source check markers
const (
	UpdateResponse       Entry = ".update_response"
	UpdateExitCode       Entry = ".update_exit_code"
	UpdateCheck          Entry = ".update_check"
	UpdatePrompt         Entry = ".update_prompt.json"
	UpdatePending        Entry = ".update_pending.json"
	UpdatePendingClaimed Entry = ".update_pending.claimed.json"
	UpdateOutput         Entry = ".update_output.txt"
	UpdateInProgress     Entry = ".icebath-update-in-progress"
	SkipUpstreamPrompt   Entry = ".skip_upstream_prompt"
	BootstrapCache       Entry = "bootstrap-cache"
	AutoPrunePrompt      Entry = ".auto_prune_prompt.json"
	AutoPruneEstimate    Entry = ".auto_prune_estimate.json"
	SourceChecks         Entry = "source-checks"
	ReleaseRuntime       Entry = "source-checks/release-runtime.json"
	Backups              Entry = "backups"
	ConfigBackups        Entry = "backups/config"
	BackupLock           Entry = ".backup.lock"
	StateSnapshots       Entry = "state-snapshots"
)

// Logs directory and the files written into it
const (
	Logs                       Entry = "logs"
	AgentLog                   Entry = "logs/agent.log"
	ErrorsLog                  Entry = "logs/errors.log"
	GatewayLog                 Entry = "logs/gateway.log"
	GUILog                     Entry = "logs/gui.log"
	DesktopLog                 Entry = "logs/desktop.log"
	GatewayStdioLog            Entry = "logs/gateway-stdio.log"
	GatewayErrorLog            Entry = "logs/gateway.error.log"
	GatewayFaultLog            Entry = "logs/gateway_faulthandler.log"
	GatewayExitDiagLog         Entry = "logs/gateway-exit-diag.log"
	GatewayShutdownDiagLog     Entry = "logs/gateway-shutdown-diag.log"
	GatewayShutdownWatchdogLog Entry = "logs/gateway-shutdown-watchdog.log"
	GatewayUpdateResumeLog     Entry = "logs/gateway-update-resume.log"
	GatewayRestartLog          Entry = "logs/gateway-restart.log"
	GatewayStartLog            Entry = "logs/gateway-start.log"
	GatewayStopLog             Entry = "logs/gateway-stop.log"
	GatewayMigrateLog          Entry = "logs/gateway-migrate.log"
	UpdateActionLog            Entry = "logs/icebath-update.log"
	ContainerBootLog           Entry = "logs/container-boot.log"
	DashboardAuthLog           Entry = "logs/dashboard-auth.log"
	DashboardRestartLog        Entry = "logs/dashboard-restart.log"
	LaunchdReloadLog           Entry = "logs/launchd-reload.log"
	MCPStderrLog               Entry = "logs/mcp-stderr.log"
	ToolCallsLog               Entry = "logs/tool_calls.log"
	TUIGatewayCrashLog         Entry = "logs/tui_gateway_crash.log"
	ScratchPruneLog            Entry = "logs/scratch-prune.log"
	LogsCurator                Entry = "logs/curator"
	ProcessOutput              Entry = "logs/process-output"
	ProcessResults             Entry = "logs/process-results"
	InterruptDebugLog          Entry = "interrupt_debug.log"
)

// Sessions, history and terminal UI state
const (
	Sessions       Entry = "sessions"
	SessionsIndex  Entry = "sessions/sessions.json"
	SavedSessions  Entry = "sessions/saved"
	SessionExports Entry = "session-exports"
	History        Entry = ".icebath_history"
	TUIThemeBoot   Entry = "tui-theme-boot.json"
	TUIWorktrees   Entry = "tui_worktrees.json"
	WorktreeRepos  Entry = "worktree_repos.json"
)

// Memory store
const (
	Memories   Entry = "memories"
	MemoryFile Entry = "memories/MEMORY.md"
	UserFile   Entry = "memories/USER.md"
)

// Skills, the skills hub, curator state and the packaged catalogs a home can carry
const (
	Skills                 Entry = "skills"
	SkillsUsage            Entry = "skills/.usage.json"
	CuratorState           Entry = "skills/.curator_state"
	CuratorLedger          Entry = "skills/.curator_ledger.jsonl"
	CuratorSuppressed      Entry = "skills/.curator_suppressed"
	SkillSnapshots         Entry = "skills/.curator_backups"
	SkillsArchive          Entry = "skills/.archive"
	BundledManifest        Entry = "skills/.bundled_manifest"
	SkillsRestoreBackups   Entry = "skills/.restore-backups"
	SkillsLocks            Entry = "skills/.locks"
	CuratorRunLock         Entry = "skills/.locks/curator-run"
	SkillsLedgerLock       Entry = "skills/.locks/ledger.lock"
	TermuxBundledSyncStamp Entry = "skills/.termux_bundled_sync_stamp"
	SkillsHub              Entry = "skills/.hub"
	SkillsHubLock          Entry = "skills/.hub/lock.json"
	SkillsHubQuarantine    Entry = "skills/.hub/quarantine"
	SkillsHubAuditLog      Entry = "skills/.hub/audit.log"
	SkillsHubTaps          Entry = "skills/.hub/taps.json"
	SkillsHubIgnore        Entry = "skills/.hub/.ignore"
	SkillsHubScanCache     Entry = "skills/.hub/scan-cache"
	SkillsHubIndexCache    Entry = "skills/.hub/index-cache"
	SkillsHubIndex         Entry = "skills/.hub/index-cache/icebath-index.json"
	SkillBundles           Entry = "skill-bundles"
	SkillsPromptSnapshot   Entry = ".skills_prompt_snapshot.json"
	NoBundledSkills        Entry = ".no-bundled-skills"
	OptionalSkills         Entry = "optional-skills"
	OptionalMCPs           Entry = "optional-mcps"
	CuratorBackups         Entry = ".curator_backups"
	CuratorBlobs           Entry = ".curator_backups/blobs"
)

// Plugins, hooks and relay plugin declarations
const (
	Plugins               Entry = "plugins"
	PluginInstallMetadata Entry = "plugins/.install-metadata.json"
	ModelProviderPlugins  Entry = "plugins/model-providers"
	PlatformPlugins       Entry = "plugins/platforms"
	PluginsBackup         Entry = "plugins-backup"
	PluginData            Entry = "plugin-data"
	PluginUpdateChecks    Entry = "plugin-update-checks"
	PluginUpdateLastRun   Entry = "plugin-update-checks/last-run"
	PluginUpdateResults   Entry = "plugin-update-checks/results.json"
	RelayPlugins          Entry = "relay-plugins.toml"
	Hooks                 Entry = "hooks"
	HookOutputs           Entry = "hook_outputs"
	ShellHooksAllowlist   Entry = "shell-hooks-allowlist.json"
)

// Scheduled jobs and the scripts they run
const (
	Cron                   Entry = "cron"
	CronJobs               Entry = "cron/jobs.json"
	CronJobsLock           Entry = "cron/.jobs.lock"
	CronOutput             Entry = "cron/output"
	CronExecutions         Entry = "cron/executions.db"
	CronDeliveries         Entry = "cron/deliveries.db"
	CronNotepad            Entry = "cron/notepad.db"
	CronSuggestions        Entry = "cron/suggestions.json"
	CronTickLock           Entry = "cron/.tick.lock"
	CronTickerHeartbeat    Entry = "cron/ticker_heartbeat"
	CronTickerLastSuccess  Entry = "cron/ticker_last_success"
	CronTickerLastError    Entry = "cron/ticker_last_error"
	CronCatchUpOccurrences Entry = "cron/catch_up_occurrences"
	CronUsageAudit         Entry = "cron/usage_audit.jsonl"
	CronForcedReleases     Entry = "cron/inflight_forced_releases.jsonl"
	CronExternalWorkers    Entry = "cron/external-workers"
	CronAdmissions         Entry = "cron/admissions"
	CronCancels            Entry = "cron/cancels"
	CronBotChatPending     Entry = "cron/bot_chat_pending"
	Scripts                Entry = "scripts"
	GmailTriageScripts     Entry = "scripts/gmail-triage"
)

// Appearance, staged input and the browser webapp records
const (
	Skins              Entry = "skins"
	DashboardThemes    Entry = "dashboard-themes"
	Locales            Entry = "locales"
	Pets               Entry = "pets"
	PetThumbs          Entry = "pets/.thumbs"
	Images             Entry = "images"
	Screenshots        Entry = "screenshots"
	Attachments        Entry = "attachments"
	ComposerPastes     Entry = "composer-pastes"
	Pastes             Entry = "pastes"
	PendingPastes      Entry = "pastes/pending.json"
	Uploads            Entry = "uploads"
	Webapp             Entry = "webapp"
	WebappRegistration Entry = "webapp/sign-in-registration.json"
	WebappSignInTokens Entry = "webapp/native-sign-in-tokens.json"
	WebappSessionToken Entry = "webapp/session-token"
)

// Tool, execution environment, proxy and language server state
const (
	Checkpoints               Entry = "checkpoints"
	CheckpointPruneStamp      Entry = "checkpoints/.last_prune"
	CheckpointStore           Entry = "checkpoints/store"
	CheckpointIndexes         Entry = "checkpoints/store/indexes"
	CheckpointProjects        Entry = "checkpoints/store/projects"
	CheckpointLedgers         Entry = "checkpoints/store/ledgers"
	CheckpointGCPending       Entry = "checkpoints/store/.gc-pending"
	CheckpointLock            Entry = ".checkpoints.lock"
	Sandboxes                 Entry = "sandboxes"
	DockerSandboxes           Entry = "sandboxes/docker"
	VercelSandboxSnapshots    Entry = "vercel_sandbox_snapshots.json"
	ModalSnapshots            Entry = "modal_snapshots.json"
	BrowserProfile            Entry = "browser-profile"
	ChromeDebug               Entry = "chrome-debug"
	ChromeDebugLaunchLog      Entry = "chrome-debug/launch-stderr.log"
	MoATraces                 Entry = "moa-traces"
	Proxy                     Entry = "proxy"
	ProxyConfig               Entry = "proxy/proxy.yaml"
	ProxyCACert               Entry = "proxy/ca.crt"
	ProxyCAKey                Entry = "proxy/ca.key"
	ProxyManagementToken      Entry = "proxy/management.token"
	ProxyMappings             Entry = "proxy/mappings.json"
	ProxyAuditLog             Entry = "proxy/audit.log"
	ProxyPID                  Entry = "proxy/iron-proxy.pid"
	ProxyNonce                Entry = "proxy/iron-proxy.nonce"
	ProxyLog                  Entry = "proxy/iron-proxy.log"
	LSP                       Entry = "lsp"
	LSPBin                    Entry = "lsp/bin"
	LSPPowerShellServices     Entry = "lsp/PowerShellEditorServices"
	LSPPowerShellSessions     Entry = "lsp/pses"
	LSPPowerShellLog          Entry = "lsp/pses/pses.log"
	Bin                       Entry = "bin"
	Lib                       Entry = "lib"
	CJKSearchLibrary          Entry = "lib/libfts5_cjk.so"
	MCPInstalls               Entry = "mcp-installs"
	MCPTokens                 Entry = "mcp-tokens"
	MCPDiscoveryLock          Entry = ".mcp-discovery.lock"
	MCPEventSubscriptions     Entry = "mcp_event_subscriptions.json"
	DiskCleanup               Entry = "disk-cleanup"
	Tmp                       Entry = "tmp"
	TeamsPipelineTmp          Entry = "tmp/teams_pipeline"
	TeamsPipelineStore        Entry = "teams_pipeline_store.json"
	Archive                   Entry = "archive"
	ArchivedWorktrees         Entry = "archive/worktree-prune"
	ConnectorDownloads        Entry = "downloads/connectors"
	WorkspaceMeetings         Entry = "workspace/meetings"
	MeetingsActive            Entry = "workspace/meetings/.active.json"
	MeetingsAuth              Entry = "workspace/meetings/auth.json"
	MeetingsNodes             Entry = "workspace/meetings/nodes.json"
	MeetingsNodeToken         Entry = "workspace/meetings/node_token.json"
	BotDesktop                Entry = "bot-desktop"
	BotDesktopLease           Entry = "bot-desktop/lease.json"
	BotDesktopLeaseLock       Entry = "bot-desktop/lease.lock"
	BotDesktopSandbox         Entry = "bot-desktop/sandbox.json"
	BotDesktopSocket          Entry = "bot-desktop/rfb.sock"
	BotDesktopLauncherPID     Entry = "bot-desktop/launcher.pid"
	BotDesktopLauncherLog     Entry = "bot-desktop/launcher.log"
	BotDesktopStartLock       Entry = "bot-desktop/start.lock"
	BotDesktopInstallLock     Entry = "bot-desktop/install.lock"
	BotDesktopDisplay         Entry = "bot-desktop/display"
	BotDesktopEnv             Entry = "bot-desktop/env"
	BotDesktopXauthority      Entry = "bot-desktop/Xauthority"
	BotDesktopXDG             Entry = "bot-desktop/xdg"
	BotDesktopActivity        Entry = "bot-desktop/activity"
	BotDesktopBrowser         Entry = "bot-desktop/browser-profile"
	Telemetry                 Entry = "telemetry"
	SharedMetrics             Entry = "telemetry/shared_metrics"
	TelemetryDB               Entry = "telemetry/shared_metrics/metrics.sqlite3"
	TelemetryOutbox           Entry = "telemetry/shared_metrics/outbox"
	TelemetryPending          Entry = "telemetry/shared_metrics/pending_updates"
	TelemetryRecorded         Entry = "telemetry/shared_metrics/recorded_updates"
	TelemetryPendingInstalls  Entry = "telemetry/shared_metrics/pending_installs"
	TelemetryRecordedInstalls Entry = "telemetry/shared_metrics/recorded_installs"
	TelemetryProcess          Entry = "telemetry/shared_metrics/process_markers"
	TelemetryProviderSetup    Entry = "telemetry/shared_metrics/provider_setup_markers"
	TelemetryOnboarding       Entry = "telemetry/shared_metrics/desktop_onboarding"
	Byterover                 Entry = "byterover"
	A2AConversations          Entry = "a2a_conversations"
	A2AAudit                  Entry = "a2a_audit.jsonl"
)

// Messaging platform credentials, cursors and caches
const (
	GoogleChatUserTokens       Entry = "google_chat_user_tokens"
	GoogleChatOAuthPending     Entry = "google_chat_user_oauth_pending"
	GoogleChatOAuthPendingFile Entry = "google_chat_user_oauth_pending.json"
	GoogleChatClientSecret     Entry = "google_chat_user_client_secret.json"
	GoogleChatThreadCounts     Entry = "google_chat_thread_counts.json"
	GoogleChatBotID            Entry = "google_chat_bot_id.json"
	SlackTokens                Entry = "slack_tokens.json"
	SlackManifest              Entry = "slack-manifest.json"
	FeishuCommentRules         Entry = "feishu_comment_rules.json"
	FeishuCommentPairing       Entry = "feishu_comment_pairing.json"
	FeishuSeenMessages         Entry = "feishu_seen_message_ids.json"
	StickerCache               Entry = "sticker_cache.json"
	ChannelDirectory           Entry = "channel_directory.json"
	ChannelAliases             Entry = "channel_aliases.json"
	BuzzCursors                Entry = "buzz"
	BuzzChannelCursors         Entry = "buzz/channel-cursors.json"
	Weixin                     Entry = "weixin"
	WeixinAccounts             Entry = "weixin/accounts"
	Platforms                  Entry = "platforms"
)

// Model catalogs and provider caches
const (
	ProviderModelsCache    Entry = "provider_models_cache.json"
	ProviderModelsLock     Entry = "provider_models_cache.json.lock"
	OllamaCloudModelsCache Entry = "ollama_cloud_models_cache.json"
	ContextLengthCache     Entry = "context_length_cache.yaml"
	ContextLengthCacheLock Entry = "context_length_cache.yaml.lock"
	ModelsDevCache         Entry = "models_dev_cache.json"
	ModelsDevCacheETag     Entry = "models_dev_cache.etag"
	ModelsDevCacheCorrupt  Entry = "models_dev_cache.json.corrupt"
	CodexAutoraiseNotice   Entry = ".codex_gpt55_autoraise_notice"
)

// Cache directory and everything regenerated on use below it
const (
	Cache                    Entry = "cache"
	Scratch                  Entry = "cache/scratch"
	Spillover                Entry = "cache/spillover"
	GeneratedMedia           Entry = "cache/generated"
	TerminalCache            Entry = "cache/terminal"
	TerminalOutput           Entry = "cache/terminal-output"
	Wakewords                Entry = "cache/wakewords"
	WhisperCpp               Entry = "cache/whisper.cpp"
	WhisperCppPartials       Entry = "cache/whisper.cpp/partials"
	PetGen                   Entry = "cache/pet-gen"
	ConnectorsCache          Entry = "cache/connectors"
	CitationLedger           Entry = "cache/citations/ledger.json"
	BlockedScripts           Entry = "cache/blocked-scripts"
	BannerSnapshot           Entry = "cache/banner_snapshot.json"
	AdvisoryBannerSeen       Entry = "cache/advisory_banner_seen"
	OAuthHealClean           Entry = "cache/oauth_heal_clean.json"
	ModelCatalog             Entry = "cache/model_catalog.json"
	RouterCatalog            Entry = "cache/router_catalog.json"
	ReasoningCaps            Entry = "cache/reasoning_caps.json"
	OpenRouterModelMetadata  Entry = "cache/openrouter_model_metadata.json"
	EndpointModelMetadata    Entry = "cache/endpoint_model_metadata.json"
	LocalEndpointProbes      Entry = "cache/local_endpoint_probes.json"
	ImageTokenCosts          Entry = "cache/image_token_costs.json"
	ToolDiscoveryCache       Entry = "cache/tool_discovery_cache.json"
	MCPSchemaCache           Entry = "cache/mcp_schema_cache.json"
	OSVCheck                 Entry = "cache/osv_check.json"
	PluginToolsetKeys        Entry = "cache/plugin_toolset_keys.json"
	PluginHost               Entry = "cache/plugin_host"
	PluginHostModelProviders Entry = "cache/plugin_host/model-providers"
	PluginCatalog            Entry = "cache/plugin-catalog.json"
	PluginPendingDelete      Entry = "cache/plugin-pending-delete"
	PlatformCABundle         Entry = "cache/platform-ca-bundle.pem"
	OpenRouterCuratedCatalog Entry = "cache/openrouter_curated_catalog.json"
	PortalRecommendedCache   Entry = "cache/portal_recommended_cache.json"
	DiscordCapabilities      Entry = "cache/discord_capabilities.json"
	WorktreeMergeVerdicts    Entry = "cache/worktree_merge_verdicts.json"
	ProjectSkillScans        Entry = "cache/project_skill_scans"
	LeftCoreNotices          Entry = "cache/left-core-notices.jsonl"
	LeftCoreNoticesLock      Entry = "cache/left-core-notices.jsonl.lock"
	SchemaColumns            Entry = "cache/schema_columns.json"
	BrowserCache             Entry = "cache/browser"
	ChromiumCache            Entry = "cache/browser/chromium"
	ChromiumRecords          Entry = "cache/browser/chromium-records"
	BrowserUse               Entry = "cache/browser-use"
	LightpandaState          Entry = "cache/browser-use/lightpanda"
	LightpandaHTTPCache      Entry = "cache/browser-use/lightpanda/http-cache"
	BrowserUseWorkspace      Entry = "cache/browser-use/workspace"
)

// Locations that exist only at the root an install shares across its profiles
const (
	Profiles                RootEntry = "profiles"
	DeletedProfiles         RootEntry = "profiles/.deleted"
	ProfileExports          RootEntry = "profile-exports"
	ActiveProfile           RootEntry = "active_profile"
	SpawnLedger             RootEntry = "spawn-ledger.json"
	SpawnLedgerLock         RootEntry = "spawn-ledger.json.lock"
	SpawnLedgerCorrupt      RootEntry = "spawn-ledger.json.corrupt"
	InstallIDFile           RootEntry = "install_id"
	InstallIDLock           RootEntry = ".install_id.lock"
	UIPreferences           RootEntry = "ui-preferences.json"
	HostedRoomsDB           RootEntry = "shared-state.db"
	WebappPortClaimLock     RootEntry = "webapp/port-claim.lock"
	WakeWordLock            RootEntry = "runtime/wake-word.lock"
	ToolStore               RootEntry = "tools"
	DownloadPartials        RootEntry = "cache/partials"
	Models                  RootEntry = "models"
	ModelAssets             RootEntry = "models/assets"
	Runtimes                RootEntry = "runtimes"
	LlamaCppRuntimes        RootEntry = "runtimes/llamacpp"
	LlamaCppAPIKey          RootEntry = "runtimes/llamacpp/.api_key"
	LlamaCppBootLock        RootEntry = "runtimes/llamacpp/boot.lock"
	LlamaCppPresets         RootEntry = "runtimes/llamacpp/presets.ini"
	LlamaCppServer          RootEntry = "runtimes/llamacpp/server.json"
	LlamaCppWindowOverrides RootEntry = "runtimes/llamacpp/window_overrides.json"
	LlamaServerLog          RootEntry = "logs/llama-server.log"
	UpdateLog               RootEntry = "logs/update.log"
	DesktopUpdateHandoffLog RootEntry = "logs/desktop-update-handoff.log"
	UpdateReceipts          RootEntry = "logs/update_receipts"
	LatestUpdateReceipt     RootEntry = "logs/update_receipts/latest.json"
	DesktopPlugins          RootEntry = "desktop-plugins"
	DesktopSSH              RootEntry = "desktop-ssh"
	DesktopInstalledApps    RootEntry = "desktop-installed-apps.json"
	FreeTier                RootEntry = "free_tier"
	FreeTierSignInOffer     RootEntry = "free_tier/sign_in_offer.json"
	FreeTierConnectorOffer  RootEntry = "free_tier/connector_offer.json"
	Shared                  RootEntry = "shared"
	PortalSharedAuth        RootEntry = "shared/portal_auth.json"
	GuestHandoff            RootEntry = "shared/guest_handoff.json"
	BotRelay                RootEntry = "bot_relay"
	BotRelayRoster          RootEntry = "bot_relay/roster.json"
	BotRelayOutbox          RootEntry = "bot_relay/outbox"
	BotRelayClaimed         RootEntry = "bot_relay/claimed"
	BotRelayReplies         RootEntry = "bot_relay/replies"
	BotRelayLocks           RootEntry = "bot_relay/locks"
)

// Kanban board store, below the kanban home that ICEBATH_KANBAN_HOME names and the root otherwise
const (
	KanbanDB            RootEntry = "kanban.db"
	KanbanScratchTip    RootEntry = ".scratch_tip_shown"
	KanbanDir           RootEntry = "kanban"
	KanbanBoards        RootEntry = "kanban/boards"
	KanbanArchivedBoard RootEntry = "kanban/boards/_archived"
	KanbanCurrentBoard  RootEntry = "kanban/current"
	KanbanWorkspaces    RootEntry = "kanban/workspaces"
	KanbanAttachments   RootEntry = "kanban/attachments"
	KanbanWorkerLogs    RootEntry = "kanban/logs"
	KanbanDispatchLock  RootEntry = "kanban/.dispatcher.lock"
	KanbanBoardExports  RootEntry = "kanban/board-exports"
)

// Root directories of re-downloadable local model runtimes that backups and profile clones leave out
func LocalRuntimeDirs() []RootEntry { return []RootEntry{Models, Runtimes} }

// Location below a home that an older layout kept under another name
type LegacyEntry int

// Directories whose populated older location wins over the current one
const (
	ImageCache LegacyEntry = iota
	AudioCache
	VideoCache
	DocumentCache
	ScreenshotCache
	WebCache
	ExecSpill
	DelegationCache
	PiperVoices
	VisionCache
	VideoAnalysisCache
	ComputerUseCache
	PairingStore
	MatrixStore
	WhatsAppSession
	WhatsAppCloudMedia
)

// Current and older relative location of each legacy entry, indexed by the entry
var legacyNames = [...]struct{ current, old Entry }{
	ImageCache:         {"cache/images", "image_cache"},
	AudioCache:         {"cache/audio", "audio_cache"},
	VideoCache:         {"cache/videos", "video_cache"},
	DocumentCache:      {"cache/documents", "document_cache"},
	ScreenshotCache:    {"cache/screenshots", "browser_screenshots"},
	WebCache:           {"cache/web", "web_cache"},
	ExecSpill:          {"cache/exec", "exec_spill"},
	DelegationCache:    {"cache/delegation", "delegation_cache"},
	PiperVoices:        {"cache/piper-voices", "piper_voices_cache"},
	VisionCache:        {"cache/vision", "temp_vision_images"},
	VideoAnalysisCache: {"cache/video", "temp_video_files"},
	ComputerUseCache:   {"cache/computer_use", "computer_use_cache"},
	PairingStore:       {"platforms/pairing", "pairing"},
	MatrixStore:        {"platforms/matrix/store", "matrix/store"},
	WhatsAppSession:    {"platforms/whatsapp/session", "whatsapp/session"},
	WhatsAppCloudMedia: {"platforms/whatsapp_cloud/media", "whatsapp_cloud/media"},
}

// Location the current layout uses
func (l LegacyEntry) Current() Entry { return legacyNames[l].current }

// Location the older layout used
func (l LegacyEntry) Old() Entry { return legacyNames[l].old }

// Older location below home when it holds anything, the current location otherwise, never migrating
func (l LegacyEntry) In(home string) string {
	if old := l.Old().In(home); legacyHasContent(old) {
		return old
	}
	return l.Current().In(home)
}

// Reports a non-directory or populated directory at p, a symlink judged by its target, an unreadable path occupied
func legacyHasContent(p string) bool {
	fi, err := os.Lstat(p)
	if err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		fi, err = os.Stat(p)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil || !fi.IsDir() {
		return true
	}
	f, err := os.Open(p)
	if err != nil {
		return true
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	return len(names) > 0 || !errors.Is(err, io.EOF)
}

// Directory a package manager may relocate, named by its override variable and its home fallback
type Packaged int

// Relocatable catalogs shipped beside the binary
const (
	BundledSkills Packaged = iota
	OptionalSkillsCatalog
	OptionalMCPsCatalog
)

// Override variable and home fallback of each packaged directory
var packagedNames = [...]struct {
	env  string
	home Entry
}{
	BundledSkills:         {"ICEBATH_BUNDLED_SKILLS", Skills},
	OptionalSkillsCatalog: {"ICEBATH_OPTIONAL_SKILLS", OptionalSkills},
	OptionalMCPsCatalog:   {"ICEBATH_OPTIONAL_MCPS", OptionalMCPs},
}

// Directory from the override variable, else def, else the fallback below home
func (p Packaged) Dir(home, def string) string {
	if v := strings.TrimSpace(os.Getenv(packagedNames[p].env)); v != "" {
		return filepath.Clean(v)
	}
	if def != "" {
		return filepath.Clean(def)
	}
	return packagedNames[p].home.In(home)
}
