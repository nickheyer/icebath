package settings

import "time"

// Let scheduled jobs create and change other scheduled jobs
var CronAllowAgentScheduling = Register(&Bool{Meta: Meta{Key: "cron.allow_agent_scheduling", Label: "Agent scheduling", Short: "Let scheduled jobs create and change other scheduled jobs.", Group: GroupChatApps, Order: 1000, Advanced: true}, Default: false})

// Run jobs missed while the app was off, once when it returns
var CronCatchUpMissed = Register(&Bool{Meta: Meta{Key: "cron.catch_up_missed", Label: "Catch up missed", Short: "Run jobs missed while the app was off, once when it returns.", Group: GroupChatApps, Order: 1010, Advanced: true}, Default: true})

// How long to wait for cleanup after a scheduled job finishes
var CronCleanupTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "cron.cleanup_timeout_seconds", Label: "Cleanup wait", Short: "How long to wait for cleanup after a scheduled job finishes.", Help: "Cleanup that runs longer is abandoned.", Group: GroupChatApps, Order: 1020, Advanced: true}, Default: 10 * time.Second})

// Send a notice to the chat when a scheduled result is delivered
var CronDeliveryNotify = Register(&Bool{Meta: Meta{Key: "cron.delivery.notify", Label: "Delivery notices", Short: "Send a notice to the chat when a scheduled result is delivered.", Group: GroupChatApps, Order: 1030, Advanced: true}, Default: true})

// Failed runs in a row before you get a nudge about a job
var CronFailureNudgeThreshold = Register(&Int{Meta: Meta{Key: "cron.failure_nudge_threshold", Label: "Failure nudge after", Short: "Failed runs in a row before you get a nudge about a job.", Group: GroupChatApps, Order: 1040, Advanced: true}, Default: 3, Unit: "failures"})

// Wait between repeat alerts for a job that keeps failing
var CronFailureRepeatAlertHours = Register(&Duration{Meta: Meta{Key: "cron.failure_repeat_alert_hours", Label: "Repeat alert gap", Short: "Wait between repeat alerts for a job that keeps failing.", Help: "Zero alerts on every failed run.", Group: GroupChatApps, Order: 1050, Advanced: true}, Default: 6 * time.Hour, Unit: time.Hour})

// Minimum time a running job gets before it counts as stuck
var CronInflightMaxMinutes = Register(&Duration{Meta: Meta{Key: "cron.inflight_max_minutes", Label: "Stuck job limit", Short: "Minimum time a running job gets before it counts as stuck.", Help: "Unset falls back to the matching environment variable, then 30 minutes.", Group: GroupChatApps, Order: 1060, Advanced: true}, Unit: time.Minute, Null: "30 minutes"})

// Most scheduled jobs that run at the same time
var CronMaxParallelJobs = Register(&Int{Meta: Meta{Key: "cron.max_parallel_jobs", Label: "Parallel jobs", Short: "Most scheduled jobs that run at the same time.", Help: "Zero also means no limit. The matching environment variable wins.", Group: GroupChatApps, Order: 1070, Advanced: true}, Unit: "jobs", Min: at(0), Null: "no limit"})

// How long to wait when sending each file from a scheduled job
var CronMediaSendTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "cron.media_send_timeout_seconds", Label: "Media send wait", Short: "How long to wait when sending each file from a scheduled job.", Group: GroupChatApps, Order: 1080, Advanced: true}, Default: 5 * time.Minute})

// Also add scheduled results to the chat session they were sent to
var CronMirrorDelivery = Register(&Bool{Meta: Meta{Key: "cron.mirror_delivery", Label: "Mirror results", Short: "Also add scheduled results to the chat session they were sent to.", Help: "A job can override this.", Group: GroupChatApps, Order: 1090, Advanced: true}, Default: false})

// How late a scheduled job may still start after its time
var CronMisfireGraceMinutes = Register(&Duration{Meta: Meta{Key: "cron.misfire_grace_minutes", Label: "Late start allowance", Short: "How late a scheduled job may still start after its time.", Group: GroupChatApps, Order: 1100, Advanced: true}, Default: 10 * time.Minute, Unit: time.Minute})

// Model scheduled jobs use when they do not pick their own
var CronModel = Register(&String{Meta: Meta{Key: "cron.model", Label: "Job model", Short: "Model scheduled jobs use when they do not pick their own.", Help: "Blank follows your main model.", Group: GroupChatApps, Order: 1110, Advanced: true}, Default: ""})

// Provider of the job model
var CronModelProvider = Register(&String{Meta: Meta{Key: "cron.model_provider", Label: "Job model provider", Short: "Provider of the job model.", Help: "Empty uses your main provider.", Group: GroupChatApps, Order: 1120, Advanced: true}, Default: ""})

// Output files kept for each scheduled job
var CronOutputRetention = Register(&Int{Meta: Meta{Key: "cron.output_retention", Label: "Kept job outputs", Short: "Output files kept for each scheduled job.", Group: GroupChatApps, Order: 1130, Advanced: true}, Default: 50, Unit: "files"})

// Check a job needs (skills, tools, logins) are ready before it runs
var CronPreflight = Register(&Bool{Meta: Meta{Key: "cron.preflight", Label: "Check before run", Short: "Check a job needs (skills, tools, logins) are ready before it runs.", Group: GroupChatApps, Order: 1140, Advanced: true}, Default: true})

// Scheduler plugin that fires jobs
var CronProvider = Register(&String{Meta: Meta{Key: "cron.provider", Label: "Scheduler plugin", Short: "Scheduler plugin that fires jobs.", Help: "Empty uses the built-in scheduler.", Group: GroupChatApps, Order: 1150, Advanced: true}, Default: ""})

// Refuse to run jobs that a restart could cut off
var CronRequireRestartSafeScope = Register(&Bool{Meta: Meta{Key: "cron.require_restart_safe_scope", Label: "Safe restart only", Short: "Refuse to run jobs that a restart could cut off.", Group: GroupChatApps, Order: 1160, Advanced: true}, Default: false})

// Retry a job later when the network or model was unreachable
var CronRetryUnreachable = Register(&Bool{Meta: Meta{Key: "cron.retry_unreachable", Label: "Retry when offline", Short: "Retry a job later when the network or model was unreachable.", Group: GroupChatApps, Order: 1170, Advanced: true}, Default: true})

// Longest a job pre-run script may run
var CronScriptTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "cron.script_timeout_seconds", Label: "Job script limit", Short: "Longest a job pre-run script may run.", Group: GroupChatApps, Order: 1180, Advanced: true}, Default: time.Hour})

// How long a job waits to open the session history
var CronSessionDbTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "cron.session_db_timeout_seconds", Label: "History open wait", Short: "How long a job waits to open the session history.", Help: "Zero waits forever. The matching environment variable wins.", Group: GroupChatApps, Order: 1190, Advanced: true}, Default: 10 * time.Second})

// How long to wait when a job sends outside a chat session
var CronStandaloneSendTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "cron.standalone_send_timeout_seconds", Label: "Direct send wait", Short: "How long to wait when a job sends outside a chat session.", Help: "Zero or less uses the default.", Group: GroupChatApps, Order: 1200, Advanced: true}, Default: time.Minute})

// Add a header and footer around scheduled results sent to chat
var CronWrapResponse = Register(&Bool{Meta: Meta{Key: "cron.wrap_response", Label: "Wrap results", Short: "Add a header and footer around scheduled results sent to chat.", Group: GroupChatApps, Order: 1210, Advanced: true}, Default: true})

// Discord: only answer in these channel IDs
var DiscordAllowedChannels = Register(&List{Meta: Meta{Key: "discord.allowed_channels", Label: "Allowed channels", Short: "Discord: only answer in these channel IDs.", Group: GroupChatApps, Order: 1220, Apply: Live, Advanced: true}})

// Discord: mention allowed users on approval requests
var DiscordApprovalMentions = Register(&Bool{Meta: Meta{Key: "discord.approval_mentions", Label: "Approval mentions", Short: "Discord: mention allowed users on approval requests.", Group: GroupChatApps, Order: 1230, Apply: Live, Advanced: true}, Default: false})

// Discord: start a thread for each new conversation
var DiscordAutoThread = Register(&Bool{Meta: Meta{Key: "discord.auto_thread", Label: "Auto threads", Short: "Discord: start a thread for each new conversation.", Group: GroupChatApps, Order: 1240, Apply: Live, Advanced: true}, Default: true})

// Discord: other bots must mention it inside their message
var DiscordBotsRequireInlineMention = Register(&Bool{Meta: Meta{Key: "discord.bots_require_inline_mention", Label: "Bots must mention", Short: "Discord: other bots must mention it inside their message.", Group: GroupChatApps, Order: 1250, Apply: Live, Advanced: true}, Default: true})

// Discord: extra instructions for chosen channels
var DiscordChannelPrompts = Register(&Map{Meta: Meta{Key: "discord.channel_prompts", Label: "Channel prompts", Short: "Discord: extra instructions for chosen channels.", Help: "Maps channel ID to the instruction text.", Group: GroupChatApps, Order: 1260, Apply: Restart, Advanced: true}})

// Discord: server whose roles allow direct messages
var DiscordDmRoleAuthGuild = Register(&String{Meta: Meta{Key: "discord.dm_role_auth_guild", Label: "DM role server", Short: "Discord: server whose roles allow direct messages.", Help: "Blank lets roles apply only inside servers.", Group: GroupChatApps, Order: 1270, Apply: Restart, Advanced: true}, Default: ""})

// Discord: start threads in channels where it replies without mention
var DiscordFreeResponseAutoThread = Register(&Bool{Meta: Meta{Key: "discord.free_response_auto_thread", Label: "Free channel threads", Short: "Discord: start threads in channels where it replies without mention.", Group: GroupChatApps, Order: 1280, Apply: Live, Advanced: true}, Default: false})

// Discord: channel IDs where it replies without a mention
var DiscordFreeResponseChannels = Register(&List{Meta: Meta{Key: "discord.free_response_channels", Label: "Free reply channels", Short: "Discord: channel IDs where it replies without a mention.", Group: GroupChatApps, Order: 1290, Apply: Live, Advanced: true}})

// Discord: read earlier messages for context when it joins a chat
var DiscordHistoryBackfill = Register(&Bool{Meta: Meta{Key: "discord.history_backfill", Label: "Read recent history", Short: "Discord: read earlier messages for context when it joins a chat.", Group: GroupChatApps, Order: 1300, Apply: Live, Advanced: true}, Default: true})

// Discord: most earlier messages read for context
var DiscordHistoryBackfillLimit = Register(&Int{Meta: Meta{Key: "discord.history_backfill_limit", Label: "History messages", Short: "Discord: most earlier messages read for context.", Group: GroupChatApps, Order: 1310, Apply: Live, Advanced: true}, Default: 50, Unit: "messages"})

// Discord: largest file it downloads from a message
var DiscordMaxAttachmentBytes = Register(&Int{Meta: Meta{Key: "discord.max_attachment_bytes", Label: "Attachment limit", Short: "Discord: largest file it downloads from a message.", Help: "Zero means no limit.", Group: GroupChatApps, Order: 1320, Apply: Live, Advanced: true}, Default: 33554432, Unit: "bytes", Min: at(0)})

// Discord: channel IDs checked for messages missed while offline
var DiscordMissedMessageBackfillChannels = Register(&List{Meta: Meta{Key: "discord.missed_message_backfill.channels", Label: "Catch-up channels", Short: "Discord: channel IDs checked for messages missed while offline.", Help: "Blank checks allowed and free reply channels.", Group: GroupChatApps, Order: 1330, Apply: Live, Advanced: true}})

// Discord: answer messages missed while the service was down
var DiscordMissedMessageBackfillEnabled = Register(&Bool{Meta: Meta{Key: "discord.missed_message_backfill.enabled", Label: "Catch up messages", Short: "Discord: answer messages missed while the service was down.", Group: GroupChatApps, Order: 1340, Apply: Restart, Advanced: true}, Default: false})

// Discord: most recent messages checked per channel when catching up
var DiscordMissedMessageBackfillLimit = Register(&Int{Meta: Meta{Key: "discord.missed_message_backfill.limit", Label: "Catch-up scan size", Short: "Discord: most recent messages checked per channel when catching up.", Group: GroupChatApps, Order: 1350, Apply: Restart, Advanced: true}, Default: 100, Unit: "messages", Min: at(1), Max: at(500)})

// Discord: tries per missed message before giving up
var DiscordMissedMessageBackfillMaxAttempts = Register(&Int{Meta: Meta{Key: "discord.missed_message_backfill.max_attempts", Label: "Catch-up retries", Short: "Discord: tries per missed message before giving up.", Group: GroupChatApps, Order: 1360, Apply: Restart, Advanced: true}, Default: 3, Min: at(1), Max: at(100)})

// Discord: most missed messages answered per catch-up
var DiscordMissedMessageBackfillMaxDispatches = Register(&Int{Meta: Meta{Key: "discord.missed_message_backfill.max_dispatches", Label: "Catch-up replies", Short: "Discord: most missed messages answered per catch-up.", Group: GroupChatApps, Order: 1370, Apply: Restart, Advanced: true}, Default: 10, Unit: "messages", Min: at(1), Max: at(100)})

// Discord: how far back missed messages are still answered
var DiscordMissedMessageBackfillWindowSeconds = Register(&Duration{Meta: Meta{Key: "discord.missed_message_backfill.window_seconds", Label: "Catch-up window", Short: "Discord: how far back missed messages are still answered.", Group: GroupChatApps, Order: 1380, Apply: Restart, Advanced: true}, Default: 6 * time.Hour, Min: at(time.Minute)})

// Discord: react to messages while working on them
var DiscordReactions = Register(&Bool{Meta: Meta{Key: "discord.reactions", Label: "Reactions", Short: "Discord: react to messages while working on them.", Group: GroupChatApps, Order: 1390, Apply: Live, Advanced: true}, Default: true})

// Discord: only answer in servers when mentioned
var DiscordRequireMention = Register(&Bool{Meta: Meta{Key: "discord.require_mention", Label: "Require mention", Short: "Discord: only answer in servers when mentioned.", Group: GroupChatApps, Order: 1400, Apply: Live, Advanced: true}, Default: true})

// Discord: server actions the agent may take
var DiscordServerActions = Register(&String{Meta: Meta{Key: "discord.server_actions", Label: "Allowed server actions", Short: "Discord: server actions the agent may take.", Help: "Comma separated action names. Empty allows all, and unknown names are ignored.", Group: GroupChatApps, Order: 1410, Advanced: true}, Default: ""})

// Discord: also require a mention inside threads
var DiscordThreadRequireMention = Register(&Bool{Meta: Meta{Key: "discord.thread_require_mention", Label: "Threads need mention", Short: "Discord: also require a mention inside threads.", Group: GroupChatApps, Order: 1420, Apply: Live, Advanced: true}, Default: false})

// Discord: leave a voice channel after this much silence
var DiscordVoiceChannelInactivityTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "discord.voice_channel_inactivity_timeout_seconds", Label: "Voice idle leave", Short: "Discord: leave a voice channel after this much silence.", Help: "Zero never leaves.", Group: GroupChatApps, Order: 1430, Apply: Restart, Advanced: true}, Default: 5 * time.Minute})

// Discord: say a short phrase when starting on a voice request
var DiscordVoiceFxAckEnabled = Register(&Bool{Meta: Meta{Key: "discord.voice_fx.ack_enabled", Label: "Spoken acknowledgements", Short: "Discord: say a short phrase when starting on a voice request.", Group: GroupChatApps, Order: 1440, Apply: Restart, Advanced: true}, Default: true})

// Discord: phrases spoken while it works on a voice request
var DiscordVoiceFxAckPhrases = Register(&List{Meta: Meta{Key: "discord.voice_fx.ack_phrases", Label: "Acknowledgement phrases", Short: "Discord: phrases spoken while it works on a voice request.", Help: "Blank list uses the built-in phrases.", Group: GroupChatApps, Order: 1450, Apply: Restart, Advanced: true}, Default: []any{"Let me look into that.", "One moment.", "Checking on that now.", "Give me a sec.", "On it."}})

// Discord: play background sound while it works on a voice request
var DiscordVoiceFxAmbientEnabled = Register(&Bool{Meta: Meta{Key: "discord.voice_fx.ambient_enabled", Label: "Ambient sound", Short: "Discord: play background sound while it works on a voice request.", Group: GroupChatApps, Order: 1460, Apply: Restart, Advanced: true}, Default: true})

// Discord: volume of the background sound
var DiscordVoiceFxAmbientGain = Register(&Float{Meta: Meta{Key: "discord.voice_fx.ambient_gain", Label: "Ambient volume", Short: "Discord: volume of the background sound.", Group: GroupChatApps, Order: 1470, Apply: Restart, Advanced: true}, Default: 0.18, Min: at(0.0)})

// Discord: sound file played while it works
var DiscordVoiceFxAmbientPath = Register(&String{Meta: Meta{Key: "discord.voice_fx.ambient_path", Label: "Ambient sound file", Short: "Discord: sound file played while it works.", Help: "Empty uses the built-in sound.", Group: GroupChatApps, Order: 1480, Apply: Restart, Advanced: true}, Default: ""})

// Discord: background volume while it speaks
var DiscordVoiceFxDuckGain = Register(&Float{Meta: Meta{Key: "discord.voice_fx.duck_gain", Label: "Ambient duck volume", Short: "Discord: background volume while it speaks.", Group: GroupChatApps, Order: 1490, Apply: Restart, Advanced: true}, Default: 0.06, Min: at(0.0)})

// Discord: use sound effects and spoken acknowledgements in voice
var DiscordVoiceFxEnabled = Register(&Bool{Meta: Meta{Key: "discord.voice_fx.enabled", Label: "Voice effects", Short: "Discord: use sound effects and spoken acknowledgements in voice.", Group: GroupChatApps, Order: 1500, Apply: Restart, Advanced: true}, Default: false})

// Discord: silence added before speech so the start is not clipped
var DiscordVoiceFxLeadSilenceMs = Register(&Duration{Meta: Meta{Key: "discord.voice_fx.lead_silence_ms", Label: "Speech lead silence", Short: "Discord: silence added before speech so the start is not clipped.", Group: GroupChatApps, Order: 1510, Apply: Restart, Advanced: true}, Default: 0, Unit: time.Millisecond})

// Discord: volume of spoken replies
var DiscordVoiceFxSpeechGain = Register(&Float{Meta: Meta{Key: "discord.voice_fx.speech_gain", Label: "Speech volume", Short: "Discord: volume of spoken replies.", Group: GroupChatApps, Order: 1520, Apply: Restart, Advanced: true}, Default: 1, Min: at(0.0)})

// Discord: shortest wait for spoken audio to finish playing
var DiscordVoicePlaybackTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "discord.voice_playback_timeout_seconds", Label: "Voice playback wait", Short: "Discord: shortest wait for spoken audio to finish playing.", Group: GroupChatApps, Order: 1530, Apply: Restart, Advanced: true}, Default: 2 * time.Minute, Min: at(time.Second)})

// Discord: reconnect when no events arrive for this long
var DiscordWebsocketEventMaxSilenceSeconds = Register(&Duration{Meta: Meta{Key: "discord.websocket_event_max_silence_seconds", Label: "Silent link limit", Short: "Discord: reconnect when no events arrive for this long.", Help: "Zero or less turns this check off.", Group: GroupChatApps, Order: 1540, Apply: Restart, Advanced: true}, Default: 4 * time.Hour})

// Discord: reconnect when Discord stops answering heartbeats
var DiscordWebsocketHeartbeatAckMaxAgeSeconds = Register(&Duration{Meta: Meta{Key: "discord.websocket_heartbeat_ack_max_age_seconds", Label: "Heartbeat reply limit", Short: "Discord: reconnect when Discord stops answering heartbeats.", Help: "Zero or less turns this check off.", Group: GroupChatApps, Order: 1550, Apply: Restart, Advanced: true}, Default: time.Minute})

// Discord: failed link checks in a row before reconnecting
var DiscordWebsocketLivenessFailureThreshold = Register(&Int{Meta: Meta{Key: "discord.websocket_liveness_failure_threshold", Label: "Link check failures", Short: "Discord: failed link checks in a row before reconnecting.", Group: GroupChatApps, Order: 1560, Apply: Restart, Advanced: true}, Default: 2, Unit: "checks"})

// Discord: time between connection health checks
var DiscordWebsocketLivenessIntervalSeconds = Register(&Duration{Meta: Meta{Key: "discord.websocket_liveness_interval_seconds", Label: "Link check interval", Short: "Discord: time between connection health checks.", Group: GroupChatApps, Order: 1570, Apply: Restart, Advanced: true}, Default: 15 * time.Second})

// Discord: reconnect when the connection is slower than this
var DiscordWebsocketMaxLatencySeconds = Register(&Duration{Meta: Meta{Key: "discord.websocket_max_latency_seconds", Label: "Link latency limit", Short: "Discord: reconnect when the connection is slower than this.", Help: "Zero or less turns this check off.", Group: GroupChatApps, Order: 1580, Apply: Restart, Advanced: true}, Default: 30 * time.Second})

// Details shown in the footer line under chat replies
var DisplayRuntimeFooterFields = Register(&List{Meta: Meta{Key: "display.runtime_footer.fields", Label: "Footer fields", Short: "Details shown in the footer line under chat replies.", Help: "Choose from model, served_model, context_pct, latency and cwd, in order.", Group: GroupChatApps, Order: 1630, Advanced: true, Platforms: true}, Default: []any{"model", "context_pct", "cwd"}})

// Let anyone message the chat apps, without an allowed list
var GatewayAllowAllUsers = Register(&Bool{Meta: Meta{Key: "gateway.allow_all_users", Label: "Allow all users", Short: "Let anyone message the chat apps, without an allowed list.", Group: GroupChatApps, Order: 1640, Apply: Live, Advanced: true}, Default: false})

// API server: longest tool output kept in saved conversation history
var GatewayApiServerHistoryToolOutputMaxChars = Register(&Int{Meta: Meta{Key: "gateway.api_server.history_tool_output_max_chars", Label: "API output cap", Short: "API server: longest tool output kept in saved conversation history.", Help: "Zero keeps full tool output.", Group: GroupChatApps, Order: 1650, Apply: Restart, Advanced: true}, Default: 0, Unit: "characters", Min: at(0)})

// API server: most requests answered at the same time
var GatewayApiServerMaxConcurrentRuns = Register(&Int{Meta: Meta{Key: "gateway.api_server.max_concurrent_runs", Label: "API parallel runs", Short: "API server: most requests answered at the same time.", Group: GroupChatApps, Order: 1660, Apply: Restart, Advanced: true}, Default: 10, Unit: "runs", Min: at(0)})

// Let the background service merge profiles into one when it can
var GatewayAutoMultiplexMigration = Register(&Bool{Meta: Meta{Key: "gateway.auto_multiplex_migration", Label: "Auto merge profiles", Short: "Let the background service merge profiles into one when it can.", Group: GroupChatApps, Order: 1670, Advanced: true}, Default: true})

// How long a chat is ignored after a bot-to-bot loop is caught
var GatewayBotLoopGuardCooldownSeconds = Register(&Duration{Meta: Meta{Key: "gateway.bot_loop_guard.cooldown_seconds", Label: "Bot loop pause", Short: "How long a chat is ignored after a bot-to-bot loop is caught.", Group: GroupChatApps, Order: 1680, Advanced: true}, Min: at(time.Second), Default: 10 * time.Minute})

// Stop replying when bots keep triggering each other in a chat
var GatewayBotLoopGuardEnabled = Register(&Bool{Meta: Meta{Key: "gateway.bot_loop_guard.enabled", Label: "Bot loop guard", Short: "Stop replying when bots keep triggering each other in a chat.", Group: GroupChatApps, Order: 1690, Advanced: true}, Default: true})

// Bot messages allowed in the window before the guard trips
var GatewayBotLoopGuardMaxEvents = Register(&Int{Meta: Meta{Key: "gateway.bot_loop_guard.max_events", Label: "Bot loop messages", Short: "Bot messages allowed in the window before the guard trips.", Group: GroupChatApps, Order: 1700, Advanced: true}, Default: 20, Unit: "messages", Min: at(1)})

// Time span over which bot messages are counted
var GatewayBotLoopGuardWindowSeconds = Register(&Duration{Meta: Meta{Key: "gateway.bot_loop_guard.window_seconds", Label: "Bot loop window", Short: "Time span over which bot messages are counted.", Group: GroupChatApps, Order: 1710, Advanced: true}, Min: at(time.Second), Default: 5 * time.Minute})

// Record sent replies so a restart does not lose or repeat them
var GatewayDeliveryLedger = Register(&Bool{Meta: Meta{Key: "gateway.delivery_ledger", Label: "Delivery records", Short: "Record sent replies so a restart does not lose or repeat them.", Group: GroupChatApps, Order: 1720, Advanced: true}, Default: true})

// Detect a frozen background service and restart it
var GatewayLoopWatchdog = Register(&Bool{Meta: Meta{Key: "gateway.loop_watchdog", Label: "Hang watchdog", Short: "Detect a frozen background service and restart it.", Group: GroupChatApps, Order: 1730, Apply: Restart, Advanced: true}, Default: true})

// Failed health probes in a row before the watchdog acts
var GatewayLoopWatchdogMaxStrikes = Register(&Int{Meta: Meta{Key: "gateway.loop_watchdog_max_strikes", Label: "Watchdog strikes", Short: "Failed health probes in a row before the watchdog acts.", Group: GroupChatApps, Order: 1740, Apply: Restart, Advanced: true}, Default: 3, Unit: "probes", Min: at(1), Max: at(1000)})

// Time between watchdog health probes
var GatewayLoopWatchdogProbeIntervalS = Register(&Duration{Meta: Meta{Key: "gateway.loop_watchdog_probe_interval_s", Label: "Watchdog interval", Short: "Time between watchdog health probes.", Group: GroupChatApps, Order: 1750, Apply: Restart, Advanced: true}, Default: 30 * time.Second, Min: at(time.Second), Max: at(time.Hour)})

// How long a health probe may take before it counts as failed
var GatewayLoopWatchdogProbeTimeoutS = Register(&Duration{Meta: Meta{Key: "gateway.loop_watchdog_probe_timeout_s", Label: "Watchdog probe wait", Short: "How long a health probe may take before it counts as failed.", Group: GroupChatApps, Order: 1760, Apply: Restart, Advanced: true}, Default: 10 * time.Second, Min: at(time.Second), Max: at(10 * time.Minute)})

// Run chat sessions in separate worker processes under a fixed policy
var GatewayManagedWorkers = Register(&Bool{Meta: Meta{Key: "gateway.managed_workers", Label: "Managed workers", Short: "Run chat sessions in separate worker processes under a fixed policy.", Group: GroupChatApps, Order: 1770, Advanced: true}, Default: false})

// Largest file accepted from a chat message
var GatewayMaxInboundMediaBytes = Register(&Int{Meta: Meta{Key: "gateway.max_inbound_media_bytes", Label: "Incoming file limit", Short: "Largest file accepted from a chat message.", Group: GroupChatApps, Order: 1780, Advanced: true}, Default: 134217728, Unit: "bytes"})

// Extra folders whose files may be sent to chats
var GatewayMediaDeliveryAllowDirs = Register(&List{Meta: Meta{Key: "gateway.media_delivery_allow_dirs", Label: "Sendable folders", Short: "Extra folders whose files may be sent to chats.", Help: "Files outside allowed folders are dropped.", Group: GroupChatApps, Order: 1790, Apply: Restart, Advanced: true}})

// Show the agent the time each chat message was sent
var GatewayMessageTimestampsEnabled = Register(&Bool{Meta: Meta{Key: "gateway.message_timestamps.enabled", Label: "Message timestamps", Short: "Show the agent the time each chat message was sent.", Group: GroupChatApps, Order: 1800, Advanced: true}, Default: false})

// Serve every profile from one background service
var GatewayMultiplexProfiles = Register(&Bool{Meta: Meta{Key: "gateway.multiplex_profiles", Label: "Merge profiles", Short: "Serve every profile from one background service.", Group: GroupChatApps, Order: 1810, Advanced: true}, Default: true})

// How long each chat app gets to connect at startup
var GatewayPlatformConnectTimeout = Register(&Duration{Meta: Meta{Key: "gateway.platform_connect_timeout", Label: "Chat connect wait", Short: "How long each chat app gets to connect at startup.", Group: GroupChatApps, Order: 1820, Apply: Restart, Advanced: true}, Default: 30 * time.Second})

// Rules that send particular chats to a particular profile
var GatewayProfileRoutes = Register(&List{Meta: Meta{Key: "gateway.profile_routes", Label: "Profile routes", Short: "Rules that send particular chats to a particular profile.", Help: "Each entry names a chat and the profile that handles it.", Group: GroupChatApps, Order: 1830, Apply: Restart, Advanced: true}})

// Proxy address for chat app connections
var GatewayProxyUrl = Register(&String{Meta: Meta{Key: "gateway.proxy_url", Label: "Chat proxy", Short: "Proxy address for chat app connections.", Help: "Empty connects directly.", Group: GroupChatApps, Order: 1840, Advanced: true}, Default: ""})

// Starts allowed in the window before startup is delayed
var GatewayRespawnStormMaxStarts = Register(&Int{Meta: Meta{Key: "gateway.respawn_storm.max_starts", Label: "Restart storm starts", Short: "Starts allowed in the window before startup is delayed.", Group: GroupChatApps, Order: 1850, Apply: Restart, Advanced: true}, Default: 5, Unit: "starts"})

// Time span over which service starts are counted
var GatewayRespawnStormWindowSeconds = Register(&Duration{Meta: Meta{Key: "gateway.respawn_storm.window_seconds", Label: "Restart storm window", Short: "Time span over which service starts are counted.", Group: GroupChatApps, Order: 1860, Apply: Restart, Advanced: true}, Default: 2 * time.Minute})

// Longest gap between restarts that still counts as a loop
var GatewayRestartLoopGuardMaxGapSeconds = Register(&Duration{Meta: Meta{Key: "gateway.restart_loop_guard.max_gap_seconds", Label: "Restart loop gap", Short: "Longest gap between restarts that still counts as a loop.", Group: GroupChatApps, Order: 1870, Advanced: true}, Default: 5 * time.Minute})

// Restarts in the window before the loop guard stops them
var GatewayRestartLoopGuardMaxRestarts = Register(&Int{Meta: Meta{Key: "gateway.restart_loop_guard.max_restarts", Label: "Restart loop count", Short: "Restarts in the window before the loop guard stops them.", Group: GroupChatApps, Order: 1880, Advanced: true}, Default: 3, Unit: "restarts"})

// Time span over which restarts are counted
var GatewayRestartLoopGuardWindowSeconds = Register(&Duration{Meta: Meta{Key: "gateway.restart_loop_guard.window_seconds", Label: "Restart loop window", Short: "Time span over which restarts are counted.", Group: GroupChatApps, Order: 1890, Advanced: true}, Default: time.Minute})

// How long the service waits with no work before it stops itself
var GatewayScaleToZeroIdleTimeoutMinutes = Register(&Duration{Meta: Meta{Key: "gateway.scale_to_zero.idle_timeout_minutes", Label: "Idle stop delay", Short: "How long the service waits with no work before it stops itself.", Group: GroupChatApps, Order: 1900, Advanced: true}, Default: 2 * time.Minute, Unit: time.Minute})

// Your answer about installing the background service
var GatewayServiceInstallChoice = Register(&Enum{Meta: Meta{Key: "gateway.service_install_choice", Label: "Service choice", Short: "Your answer about installing the background service.", Help: "Set by setup. Change it only to be asked again.", Group: GroupChatApps, Order: 1910, Advanced: true}, Default: "", Values: []Choice{{Value: "install", Label: "Install", Meaning: "Run as a login service."}, {Value: "decline", Label: "Decline", Meaning: "Do not install a service."}}, Null: "not asked yet"})

// How long a running turn gets to stop when the service is signaled
var GatewaySignalInterruptGraceTimeout = Register(&Duration{Meta: Meta{Key: "gateway.signal_interrupt_grace_timeout", Label: "Stop grace time", Short: "How long a running turn gets to stop when the service is signaled.", Group: GroupChatApps, Order: 1920, Advanced: true}, Default: time.Second})

// Run this profile's own background service instead of sharing one
var GatewayStandalone = Register(&Bool{Meta: Meta{Key: "gateway.standalone", Label: "Own service", Short: "Run this profile's own background service instead of sharing one.", Help: "Older option. Merge profiles is the better choice.", Group: GroupChatApps, Order: 1930, Advanced: true}, Default: false})

// Restart the service if it never finishes starting
var GatewayStartupWatchdog = Register(&Bool{Meta: Meta{Key: "gateway.startup_watchdog", Label: "Startup watchdog", Short: "Restart the service if it never finishes starting.", Group: GroupChatApps, Order: 1940, Apply: Restart, Advanced: true, Env: "ICEBATH_STARTUP_WATCHDOG"}, Default: true})

// How long startup may take before the watchdog acts
var GatewayStartupWatchdogTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "gateway.startup_watchdog_timeout_seconds", Label: "Startup time limit", Short: "How long startup may take before the watchdog acts.", Group: GroupChatApps, Order: 1950, Apply: Restart, Advanced: true, Env: "ICEBATH_STARTUP_WATCHDOG_TIMEOUT_S"}, Default: 5 * time.Minute})

// Refuse to send files unless their folder is explicitly allowed
var GatewayStrict = Register(&Bool{Meta: Meta{Key: "gateway.strict", Label: "Strict file sending", Short: "Refuse to send files unless their folder is explicitly allowed.", Group: GroupChatApps, Order: 1960, Apply: Restart, Advanced: true}, Default: false})

// Seconds between liveness pings to the system service manager
var GatewaySystemdWatchdogSeconds = Register(&Duration{Meta: Meta{Key: "gateway.systemd_watchdog_seconds", Label: "System watchdog time", Short: "Seconds between liveness pings to the system service manager.", Group: GroupChatApps, Order: 1970, Apply: Restart, Advanced: true}, Max: at(2147483647 * time.Second), Null: "off"})

// Honor proxy and certificate environment variables for chat apps
var GatewayTrustEnv = Register(&Bool{Meta: Meta{Key: "gateway.trust_env", Label: "Use proxy settings", Short: "Honor proxy and certificate environment variables for chat apps.", Group: GroupChatApps, Order: 1980, Advanced: true}, Default: true})

// Allow sending just-created files from outside allowed folders
var GatewayTrustRecentFiles = Register(&Bool{Meta: Meta{Key: "gateway.trust_recent_files", Label: "Trust recent files", Short: "Allow sending just-created files from outside allowed folders.", Group: GroupChatApps, Order: 1990, Apply: Restart, Advanced: true}, Default: true})

// How recent a file must be to count as just created
var GatewayTrustRecentFilesSeconds = Register(&Duration{Meta: Meta{Key: "gateway.trust_recent_files_seconds", Label: "Recent file window", Short: "How recent a file must be to count as just created.", Group: GroupChatApps, Order: 2000, Apply: Restart, Advanced: true}, Default: 10 * time.Minute})

// A hand-started service quits after this long with no work
var GatewayUnmanagedIdleExitSeconds = Register(&Duration{Meta: Meta{Key: "gateway.unmanaged_idle_exit_seconds", Label: "Idle exit time", Short: "A hand-started service quits after this long with no work.", Help: "Zero never quits.", Group: GroupChatApps, Order: 2010, Advanced: true}, Default: 10 * time.Minute})

// Keep a readable index file of chat sessions
var GatewayWriteSessionsJson = Register(&Bool{Meta: Meta{Key: "gateway.write_sessions_json", Label: "Write session index", Short: "Keep a readable index file of chat sessions.", Group: GroupChatApps, Order: 2020, Apply: Restart, Advanced: true}, Default: true})

// Most messages the agents may post in one room discussion
var HostedRoomsDiscussionMaxMessages = Register(&Int{Meta: Meta{Key: "hosted_rooms.discussion.max_messages", Label: "Room reply limit", Short: "Most messages the agents may post in one room discussion.", Group: GroupChatApps, Order: 2030, Advanced: true}, Default: 10, Unit: "messages", Min: at(1)})

// Most rounds of replies in one room discussion
var HostedRoomsDiscussionMaxRounds = Register(&Int{Meta: Meta{Key: "hosted_rooms.discussion.max_rounds", Label: "Room discussion rounds", Short: "Most rounds of replies in one room discussion.", Group: GroupChatApps, Order: 2040, Advanced: true}, Default: 3, Unit: "rounds", Min: at(1)})

// Longest pause before a reply when custom pacing is on
var HumanDelayMaxMs = Register(&Duration{Meta: Meta{Key: "human_delay.max_ms", Label: "Longest typing pause", Short: "Longest pause before a reply when custom pacing is on.", Group: GroupChatApps, Order: 2050, Apply: Restart, Advanced: true}, Default: 2500 * time.Millisecond, Unit: time.Millisecond})

// Shortest pause before a reply when custom pacing is on
var HumanDelayMinMs = Register(&Duration{Meta: Meta{Key: "human_delay.min_ms", Label: "Shortest typing pause", Short: "Shortest pause before a reply when custom pacing is on.", Group: GroupChatApps, Order: 2060, Apply: Restart, Advanced: true}, Default: 800 * time.Millisecond, Unit: time.Millisecond})

// Matrix: only answer in these room IDs
var MatrixAllowedRooms = Register(&List{Meta: Meta{Key: "matrix.allowed_rooms", Label: "Allowed rooms", Short: "Matrix: only answer in these room IDs.", Group: GroupChatApps, Order: 2070, Apply: Restart, Advanced: true}})

// Matrix: room IDs where it replies without a mention
var MatrixFreeResponseRooms = Register(&List{Meta: Meta{Key: "matrix.free_response_rooms", Label: "Free reply rooms", Short: "Matrix: room IDs where it replies without a mention.", Group: GroupChatApps, Order: 2080, Apply: Restart, Advanced: true}})

// Matrix: only answer in rooms when mentioned
var MatrixRequireMention = Register(&Bool{Meta: Meta{Key: "matrix.require_mention", Label: "Require mention", Short: "Matrix: only answer in rooms when mentioned.", Group: GroupChatApps, Order: 2090, Apply: Restart, Advanced: true}, Default: true})

// Mattermost: only answer in these channel IDs
var MattermostAllowedChannels = Register(&List{Meta: Meta{Key: "mattermost.allowed_channels", Label: "Allowed channels", Short: "Mattermost: only answer in these channel IDs.", Group: GroupChatApps, Order: 2100, Apply: Live, Advanced: true}})

// Mattermost: extra instructions for chosen channels
var MattermostChannelPrompts = Register(&Map{Meta: Meta{Key: "mattermost.channel_prompts", Label: "Channel prompts", Short: "Mattermost: extra instructions for chosen channels.", Help: "Maps channel ID to the instruction text.", Group: GroupChatApps, Order: 2110, Apply: Restart, Advanced: true}})

// Mattermost: channel IDs where it replies without a mention
var MattermostFreeResponseChannels = Register(&List{Meta: Meta{Key: "mattermost.free_response_channels", Label: "Free reply channels", Short: "Mattermost: channel IDs where it replies without a mention.", Group: GroupChatApps, Order: 2120, Apply: Live, Advanced: true}})

// Mattermost: only answer in channels when mentioned
var MattermostRequireMention = Register(&Bool{Meta: Meta{Key: "mattermost.require_mention", Label: "Require mention", Short: "Mattermost: only answer in channels when mentioned.", Group: GroupChatApps, Order: 2130, Apply: Live, Advanced: true}, Default: true})

// Replace or extend the built-in tips about each chat app
var PlatformHints = Register(&Map{Meta: Meta{Key: "platform_hints", Label: "Platform hints", Short: "Replace or extend the built-in tips about each chat app.", Help: "Keyed by app name. Each entry replaces or extends that app's built-in hint.", Group: GroupChatApps, Order: 2140, Apply: NewSession, Advanced: true}})

// Slack: only answer in these channel IDs
var SlackAllowedChannels = Register(&List{Meta: Meta{Key: "slack.allowed_channels", Label: "Allowed channels", Short: "Slack: only answer in these channel IDs.", Group: GroupChatApps, Order: 2150, Apply: Live, Advanced: true}})

// Slack: extra instructions for chosen channels
var SlackChannelPrompts = Register(&Map{Meta: Meta{Key: "slack.channel_prompts", Label: "Channel prompts", Short: "Slack: extra instructions for chosen channels.", Help: "Maps channel ID to the instruction text.", Group: GroupChatApps, Order: 2160, Apply: Restart, Advanced: true}})

// Slack: channel IDs where it replies without a mention
var SlackFreeResponseChannels = Register(&List{Meta: Meta{Key: "slack.free_response_channels", Label: "Free reply channels", Short: "Slack: channel IDs where it replies without a mention.", Group: GroupChatApps, Order: 2170, Apply: Live, Advanced: true}})

// Slack: ignore messages that mention someone else but not it
var SlackIgnoreOtherUserMentions = Register(&Bool{Meta: Meta{Key: "slack.ignore_other_user_mentions", Label: "Skip others' mentions", Short: "Slack: ignore messages that mention someone else but not it.", Group: GroupChatApps, Order: 2180, Apply: Live, Advanced: true}, Default: false})

// Slack: only answer in channels when mentioned
var SlackRequireMention = Register(&Bool{Meta: Meta{Key: "slack.require_mention", Label: "Require mention", Short: "Slack: only answer in channels when mentioned.", Group: GroupChatApps, Order: 2190, Apply: Live, Advanced: true}, Default: true})

// Slack: channel IDs that always need a mention
var SlackRequireMentionChannels = Register(&List{Meta: Meta{Key: "slack.require_mention_channels", Label: "Mention-only channels", Short: "Slack: channel IDs that always need a mention.", Group: GroupChatApps, Order: 2200, Apply: Live, Advanced: true}})

// Slack: also require a mention inside threads
var SlackThreadRequireMention = Register(&Bool{Meta: Meta{Key: "slack.thread_require_mention", Label: "Threads need mention", Short: "Slack: also require a mention inside threads.", Group: GroupChatApps, Order: 2210, Apply: Live, Advanced: true}, Default: false})

// Characters gathered before each streamed update is shown
var StreamingBufferThreshold = Register(&Int{Meta: Meta{Key: "streaming.buffer_threshold", Label: "Stream batch size", Short: "Characters gathered before each streamed update is shown.", Group: GroupChatApps, Order: 2220, Apply: Restart, Advanced: true}, Default: 24, Unit: "characters"})

// Marker shown at the end of a reply while it streams
var StreamingCursor = Register(&String{Meta: Meta{Key: "streaming.cursor", Label: "Stream cursor", Short: "Marker shown at the end of a reply while it streams.", Group: GroupChatApps, Order: 2230, Apply: Restart, Advanced: true}, Default: " ▉"})

// Seconds between updates to a streamed reply
var StreamingEditInterval = Register(&Float{Meta: Meta{Key: "streaming.edit_interval", Label: "Stream update gap", Short: "Seconds between updates to a streamed reply.", Group: GroupChatApps, Order: 2240, Apply: Restart, Advanced: true}, Default: 0.8, Unit: "seconds", Min: at(0.0)})

// Show chat replies as they are written instead of all at once
var StreamingEnabled = Register(&Bool{Meta: Meta{Key: "streaming.enabled", Label: "Stream replies", Short: "Show chat replies as they are written instead of all at once.", Help: "Each chat app can switch it off separately.", Group: GroupChatApps, Order: 2250, Apply: Restart, Advanced: true}, Default: false})

// Send the final reply as a new message after this long
var StreamingFreshFinalAfterSeconds = Register(&Duration{Meta: Meta{Key: "streaming.fresh_final_after_seconds", Label: "Fresh final reply", Short: "Send the final reply as a new message after this long.", Help: "Zero edits the streamed message instead.", Group: GroupChatApps, Order: 2260, Apply: Restart, Advanced: true}, Default: 0})

// How streamed replies are delivered
var StreamingTransport = Register(&Enum{Meta: Meta{Key: "streaming.transport", Label: "Stream method", Short: "How streamed replies are delivered.", Group: GroupChatApps, Order: 2270, Apply: Restart, Advanced: true}, Default: "auto", Values: []Choice{{Value: "auto", Label: "Auto", Meaning: "Best method the chat app supports."}, {Value: "edit", Label: "Edit", Meaning: "Keep editing one message."}, {Value: "draft", Label: "Draft", Meaning: "Use drafts where supported."}, {Value: "off", Label: "Off", Meaning: "No streaming anywhere."}}})

// Telegram: only answer in these chat IDs
var TelegramAllowedChats = Register(&List{Meta: Meta{Key: "telegram.allowed_chats", Label: "Allowed chats", Short: "Telegram: only answer in these chat IDs.", Group: GroupChatApps, Order: 2280, Apply: Live, Advanced: true}})

// Telegram: extra instructions for chosen chats
var TelegramChannelPrompts = Register(&Map{Meta: Meta{Key: "telegram.channel_prompts", Label: "Channel prompts", Short: "Telegram: extra instructions for chosen chats.", Help: "Maps chat ID to the instruction text.", Group: GroupChatApps, Order: 2290, Apply: Restart, Advanced: true}})

// Telegram: allow rich formatting in Chinese, Japanese and Korean text
var TelegramExtraAllowCjkRichMessages = Register(&Bool{Meta: Meta{Key: "telegram.extra.allow_cjk_rich_messages", Label: "CJK rich text", Short: "Telegram: allow rich formatting in Chinese, Japanese and Korean text.", Group: GroupChatApps, Order: 2300, Apply: Restart, Advanced: true}, Default: false})

// Telegram: move a direct message conversation into its own topic
var TelegramExtraDmTopicAutoTransfer = Register(&Bool{Meta: Meta{Key: "telegram.extra.dm_topic_auto_transfer", Label: "Move DM topics", Short: "Telegram: move a direct message conversation into its own topic.", Group: GroupChatApps, Order: 2310, Apply: Restart, Advanced: true}, Default: false})

// Telegram: use rich formatting in streamed drafts
var TelegramExtraRichDrafts = Register(&Bool{Meta: Meta{Key: "telegram.extra.rich_drafts", Label: "Rich drafts", Short: "Telegram: use rich formatting in streamed drafts.", Group: GroupChatApps, Order: 2320, Apply: Restart, Advanced: true}, Default: false})

// Telegram: use rich formatting in replies
var TelegramExtraRichMessages = Register(&Bool{Meta: Meta{Key: "telegram.extra.rich_messages", Label: "Rich messages", Short: "Telegram: use rich formatting in replies.", Group: GroupChatApps, Order: 2330, Apply: Restart, Advanced: true}, Default: false})

// Telegram: react to messages while working on them
var TelegramReactions = Register(&Bool{Meta: Meta{Key: "telegram.reactions", Label: "Reactions", Short: "Telegram: react to messages while working on them.", Group: GroupChatApps, Order: 2340, Apply: Live, Advanced: true}, Default: false})

// What the background service does once every chat app has disconnected
var GatewayOnAllAdaptersDown = Register(&Enum{Meta: Meta{Key: "gateway.on_all_adapters_down", Label: "All chats down", Short: "What the background service does once every chat app has dropped.", Group: GroupChatApps, Order: 2000, Advanced: true, Apply: Restart}, Default: "exit", Values: []Choice{{Value: "exit", Label: "Exit", Meaning: "Stop the background service."}, {Value: "stay_alive", Label: "Keep running", Meaning: "Keep running and keep reconnecting."}}})

// Address other machines use to reach this machine's shared rooms
var GatewayRoomLinkURL = Register(&String{Meta: Meta{Key: "gateway.room_link_url", Label: "Room link address", Short: "Address other machines use to reach the rooms shared from here.", Help: "Plain http is allowed only on this machine. A named profile without its own takes the main profile's.", Group: GroupChatApps, Order: 2010, Advanced: true}, Null: "rooms are not offered to other machines"})

// Telegram: answer in groups only when mentioned
var TelegramRequireMention = Register(&Bool{Meta: Meta{Key: "telegram.require_mention", Label: "Require mention", Short: "Telegram: answer in groups only when someone mentions the bot.", Group: GroupChatApps, Order: 2340, Advanced: true}, Default: false})

// Telegram: words that count as a mention besides the bot's name
var TelegramMentionPatterns = Register(&List{Meta: Meta{Key: "telegram.mention_patterns", Label: "Mention words", Short: "Telegram: words that count as a mention besides the bot's name.", Help: "Each item is a pattern matched against the message text.", Group: GroupChatApps, Order: 2340, Advanced: true}})

// Telegram: a mention of another bot means the message is not for this one
var TelegramExclusiveBotMentions = Register(&Bool{Meta: Meta{Key: "telegram.exclusive_bot_mentions", Label: "Other bot mentions", Short: "Telegram: skip messages that mention only another bot.", Group: GroupChatApps, Order: 2340, Advanced: true}, Default: true})

// Telegram: which messages from other bots it answers
var TelegramAllowBots = Register(&Enum{Meta: Meta{Key: "telegram.allow_bots", Label: "Answer bots", Short: "Telegram: which messages from other bots it answers.", Group: GroupChatApps, Order: 2340, Advanced: true}, Default: "none", Values: []Choice{{Value: "none", Label: "None", Meaning: "Ignore other bots."}, {Value: "mentions", Label: "Mentions", Meaning: "Answer bots that mention it."}, {Value: "all", Label: "All", Meaning: "Answer every bot."}}})

// Telegram: other bots must mention it to get an answer
var TelegramBotsRequireMention = Register(&Bool{Meta: Meta{Key: "telegram.bots_require_mention", Label: "Bots must mention", Short: "Telegram: other bots must mention it to get an answer.", Group: GroupChatApps, Order: 2340, Advanced: true}, Default: false})

// Telegram: answer people who reach the bot as a guest in another chat
var TelegramGuestMode = Register(&Bool{Meta: Meta{Key: "telegram.guest_mode", Label: "Guest mode", Short: "Telegram: answer when someone brings the bot into a chat as a guest.", Group: GroupChatApps, Order: 2340, Advanced: true}, Default: false})

// Telegram: read group messages that do not mention it, without answering
var TelegramObserveUnmentionedGroupMessages = Register(&Bool{Meta: Meta{Key: "telegram.observe_unmentioned_group_messages", Label: "Read group talk", Short: "Telegram: keep group messages that do not mention it as context.", Help: "It still answers only when mentioned.", Group: GroupChatApps, Order: 2340, Advanced: true}, Default: false})

// Telegram: chats where it answers without a mention
var TelegramFreeResponseChats = Register(&List{Meta: Meta{Key: "telegram.free_response_chats", Label: "Free reply chats", Short: "Telegram: chats where it answers without being mentioned.", Group: GroupChatApps, Order: 2340, Advanced: true}})

// Telegram: forum topics where it answers without a mention
var TelegramFreeResponseTopics = Register(&List{Meta: Meta{Key: "telegram.free_response_topics", Label: "Free reply topics", Short: "Telegram: forum topics where it answers without being mentioned.", Help: "Each item is chat:topic, such as -100123:7.", Group: GroupChatApps, Order: 2340, Advanced: true}})

// Telegram: the only forum topics it answers in
var TelegramAllowedTopics = Register(&List{Meta: Meta{Key: "telegram.allowed_topics", Label: "Allowed topics", Short: "Telegram: the only forum topics it answers in.", Help: "Each item is chat:topic. Empty allows every topic.", Group: GroupChatApps, Order: 2340, Advanced: true}})

// Telegram: forum topics it never reads
var TelegramIgnoredThreads = Register(&List{Meta: Meta{Key: "telegram.ignored_threads", Label: "Ignored topics", Short: "Telegram: forum topic numbers it never reads.", Group: GroupChatApps, Order: 2340, Advanced: true}})

// Telegram: people allowed to talk to it in private chats
var TelegramAllowFrom = Register(&List{Meta: Meta{Key: "telegram.allow_from", Label: "Allowed people", Short: "Telegram: user IDs allowed to talk to it; * allows everyone.", Help: "Empty leaves access to pairing and the shared allow list.", Group: GroupChatApps, Order: 2340, Advanced: true}})

// Telegram: people allowed to talk to it in groups
var TelegramGroupAllowFrom = Register(&List{Meta: Meta{Key: "telegram.group_allow_from", Label: "Allowed in groups", Short: "Telegram: user IDs allowed to talk to it in groups.", Group: GroupChatApps, Order: 2340, Advanced: true}})

// Telegram: groups where everyone may talk to it
var TelegramGroupAllowedChats = Register(&List{Meta: Meta{Key: "telegram.group_allowed_chats", Label: "Open groups", Short: "Telegram: group chat IDs where everyone may talk to it.", Group: GroupChatApps, Order: 2340, Advanced: true}})

// Telegram: whether replies make the phone notify
var TelegramNotifications = Register(&Enum{Meta: Meta{Key: "telegram.notifications", Label: "Reply alerts", Short: "Telegram: which replies make the phone buzz.", Group: GroupChatApps, Order: 2340, Advanced: true}, Default: "important", Values: []Choice{{Value: "important", Label: "Important", Meaning: "Only the final answer and questions."}, {Value: "all", Label: "All", Meaning: "Every message, progress included."}}})

// Discord: people allowed to talk to it
var DiscordAllowFrom = Register(&List{Meta: Meta{Key: "discord.allow_from", Label: "Allowed people", Short: "Discord: user IDs or names allowed to talk to it; * allows everyone.", Help: "Empty leaves access to pairing and the shared allow list.", Group: GroupChatApps, Order: 1220, Advanced: true}})

// Discord: server roles whose members may talk to it
var DiscordAllowedRoles = Register(&List{Meta: Meta{Key: "discord.allowed_roles", Label: "Allowed roles", Short: "Discord: server role IDs whose members may talk to it.", Group: GroupChatApps, Order: 1220, Advanced: true}})

// Discord: let everyone talk to it
var DiscordAllowAllUsers = Register(&Bool{Meta: Meta{Key: "discord.allow_all_users", Label: "Allow everyone", Short: "Discord: let anyone who can see the bot talk to it.", Group: GroupChatApps, Order: 1220, Advanced: true}, Default: false})

// Discord: which messages from other bots it answers
var DiscordAllowBots = Register(&Enum{Meta: Meta{Key: "discord.allow_bots", Label: "Answer bots", Short: "Discord: which messages from other bots it answers.", Group: GroupChatApps, Order: 1220, Advanced: true}, Default: "none", Values: []Choice{{Value: "none", Label: "None", Meaning: "Ignore other bots."}, {Value: "mentions", Label: "Mentions", Meaning: "Answer bots that mention it."}, {Value: "all", Label: "All", Meaning: "Answer every bot."}}})

// Discord: channels it never reads
var DiscordIgnoredChannels = Register(&List{Meta: Meta{Key: "discord.ignored_channels", Label: "Ignored channels", Short: "Discord: channel IDs or names it never reads.", Group: GroupChatApps, Order: 1220, Advanced: true}})

// Discord: channels where it answers in place instead of opening a thread
var DiscordNoThreadChannels = Register(&List{Meta: Meta{Key: "discord.no_thread_channels", Label: "No thread channels", Short: "Discord: channels where it answers in place, without a thread.", Group: GroupChatApps, Order: 1220, Advanced: true}})

// Matrix: address of the homeserver the bot account lives on
var MatrixHomeserver = Register(&String{Meta: Meta{Key: "matrix.homeserver", Label: "Homeserver", Short: "Matrix: address of the server the bot account lives on.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}})

// Matrix: user ID of the bot account
var MatrixUserID = Register(&String{Meta: Meta{Key: "matrix.user_id", Label: "Bot user ID", Short: "Matrix: user ID of the bot account, such as @bot:example.org.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}})

// Matrix: device ID the bot signs in as
var MatrixDeviceID = Register(&String{Meta: Meta{Key: "matrix.device_id", Label: "Device ID", Short: "Matrix: device ID the bot signs in as, kept for encryption.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}})

// Matrix: people allowed to talk to it
var MatrixAllowedUsers = Register(&List{Meta: Meta{Key: "matrix.allowed_users", Label: "Allowed people", Short: "Matrix: user IDs allowed to talk to it; * allows everyone.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}})

// Matrix: start a thread for each new conversation
var MatrixAutoThread = Register(&Bool{Meta: Meta{Key: "matrix.auto_thread", Label: "Auto threads", Short: "Matrix: answer each new conversation in its own thread.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}, Default: true})

// Matrix: open a thread when someone mentions it in a direct chat
var MatrixDmMentionThreads = Register(&Bool{Meta: Meta{Key: "matrix.dm_mention_threads", Label: "Threads in DMs", Short: "Matrix: open a thread when mentioned in a direct chat.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}, Default: false})

// Matrix: what one conversation covers
var MatrixSessionScope = Register(&Enum{Meta: Meta{Key: "matrix.session_scope", Label: "Conversation scope", Short: "Matrix: whether a conversation covers a whole room or one thread.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}, Default: "auto", Values: []Choice{{Value: "auto", Label: "Auto", Meaning: "A thread when there is one, else the room."}, {Value: "room", Label: "Room", Meaning: "One conversation per room."}, {Value: "thread", Label: "Thread", Meaning: "One conversation per thread."}}})

// Matrix: read notice messages from other bots
var MatrixProcessNotices = Register(&Bool{Meta: Meta{Key: "matrix.process_notices", Label: "Read notices", Short: "Matrix: treat notice messages, which bots send, as messages to it.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}, Default: false})

// Matrix: senders it ignores, by pattern
var MatrixIgnoreUserPatterns = Register(&List{Meta: Meta{Key: "matrix.ignore_user_patterns", Label: "Ignored senders", Short: "Matrix: patterns of user IDs whose messages it ignores.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}})

// Matrix: longest message it sends before splitting
var MatrixMaxMessageLength = Register(&Int{Meta: Meta{Key: "matrix.max_message_length", Label: "Message length", Short: "Matrix: longest message it sends before splitting a reply.", Help: "Unset uses the length the server allows.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}, Unit: "characters", Null: "the server's limit"})

// WhatsApp: who may talk to it in direct chats
var WhatsappDmPolicy = Register(&Enum{Meta: Meta{Key: "whatsapp.dm_policy", Label: "Direct chat access", Short: "WhatsApp: who may talk to it in direct chats.", Group: GroupChatApps, Order: 2350, Advanced: true, Apply: Restart}, Default: "pairing", Values: whatsappPolicyChoices})

// WhatsApp: who may talk to it in groups
var WhatsappGroupPolicy = Register(&Enum{Meta: Meta{Key: "whatsapp.group_policy", Label: "Group access", Short: "WhatsApp: who may talk to it in groups.", Group: GroupChatApps, Order: 2350, Advanced: true, Apply: Restart}, Default: "pairing", Values: whatsappPolicyChoices})

// WhatsApp: people allowed to talk to it in direct chats
var WhatsappAllowFrom = Register(&List{Meta: Meta{Key: "whatsapp.allow_from", Label: "Allowed people", Short: "WhatsApp: phone numbers allowed to talk to it; * allows everyone.", Group: GroupChatApps, Order: 2350, Advanced: true}})

// WhatsApp: people allowed to talk to it in groups
var WhatsappGroupAllowFrom = Register(&List{Meta: Meta{Key: "whatsapp.group_allow_from", Label: "Allowed in groups", Short: "WhatsApp: group IDs or numbers allowed to talk to it in groups.", Group: GroupChatApps, Order: 2350, Advanced: true}})

// WhatsApp: answer in groups only when mentioned
var WhatsappRequireMention = Register(&Bool{Meta: Meta{Key: "whatsapp.require_mention", Label: "Require mention", Short: "WhatsApp: answer in groups only when someone mentions it.", Group: GroupChatApps, Order: 2350, Advanced: true}, Default: false})

// WhatsApp: words that count as a mention
var WhatsappMentionPatterns = Register(&List{Meta: Meta{Key: "whatsapp.mention_patterns", Label: "Mention words", Short: "WhatsApp: words that count as a mention besides its name.", Group: GroupChatApps, Order: 2350, Advanced: true}})

// WhatsApp: chats where it answers without a mention
var WhatsappFreeResponseChats = Register(&List{Meta: Meta{Key: "whatsapp.free_response_chats", Label: "Free reply chats", Short: "WhatsApp: chats where it answers without being mentioned.", Group: GroupChatApps, Order: 2350, Advanced: true}})

// Buzz: address of the relay the bot talks through
var BuzzRelayURL = Register(&String{Meta: Meta{Key: "buzz.relay_url", Label: "Relay address", Short: "Buzz: address of the relay the bot talks through.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart}})

// Buzz: the buzz program it runs
var BuzzCliPath = Register(&String{Meta: Meta{Key: "buzz.cli_path", Label: "Buzz program", Short: "Buzz: path of the buzz program; empty finds it on the PATH.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart}})

// Buzz: how it receives messages
var BuzzTransport = Register(&Enum{Meta: Meta{Key: "buzz.transport", Label: "Connection type", Short: "Buzz: how it receives messages from the relay.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart}, Default: "auto", Values: []Choice{{Value: "auto", Label: "Auto", Meaning: "A live connection, polling when it fails."}, {Value: "websocket", Label: "Live", Meaning: "Only a live connection."}, {Value: "poll", Label: "Poll", Meaning: "Ask the relay every few seconds."}}})

// Buzz: seconds between checks when polling
var BuzzPollInterval = Register(&Float{Meta: Meta{Key: "buzz.poll_interval", Label: "Poll interval", Short: "Buzz: seconds between checks for new messages when polling.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart}, Unit: "seconds", Null: "the built-in interval"})

// Buzz: channels it listens in
var BuzzChannels = Register(&List{Meta: Meta{Key: "buzz.channels", Label: "Channels", Short: "Buzz: channels it listens in.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart}})

// Buzz: people allowed to talk to it
var BuzzAllowedUsers = Register(&List{Meta: Meta{Key: "buzz.allowed_users", Label: "Allowed people", Short: "Buzz: public keys of people allowed to talk to it.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart}})

// Buzz: people whose messages it only reacts to
var BuzzReactionOnlyUsers = Register(&List{Meta: Meta{Key: "buzz.reaction_only_users", Label: "React only to", Short: "Buzz: public keys whose messages get a reaction but no reply.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart}})

// Buzz: let everyone talk to it
var BuzzAllowAllUsers = Register(&Bool{Meta: Meta{Key: "buzz.allow_all_users", Label: "Allow everyone", Short: "Buzz: let anyone in its channels talk to it.", Group: GroupChatApps, Order: 1100, Advanced: true}, Default: false})

// Buzz: answer in channels only when mentioned
var BuzzRequireMention = Register(&Bool{Meta: Meta{Key: "buzz.require_mention", Label: "Require mention", Short: "Buzz: answer in channels only when someone mentions it.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart}, Default: true})

// Buzz: answer in a thread under the message
var BuzzReplyInThread = Register(&Bool{Meta: Meta{Key: "buzz.reply_in_thread", Label: "Reply in thread", Short: "Buzz: answer in a thread under the message; off answers in place.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart}, Default: true})

// Buzz: which reply parts point back at the message
var BuzzReplyToMode = Register(&Enum{Meta: Meta{Key: "buzz.reply_to_mode", Label: "Reply quoting", Short: "Buzz: which parts of a reply point back at your message.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart}, Default: "first", Values: []Choice{{Value: "first", Label: "First", Meaning: "Only the first part."}, {Value: "all", Label: "All", Meaning: "Every part."}, {Value: "off", Label: "Off", Meaning: "None."}}})

// WeCom: ID of the smart robot
var WecomBotID = Register(&String{Meta: Meta{Key: "wecom.bot_id", Label: "Robot ID", Short: "WeCom: ID of the smart robot the bot runs as.", Group: GroupChatApps, Order: 2400, Advanced: true, Apply: Restart}})

// WeCom: live connection address
var WecomWebsocketURL = Register(&String{Meta: Meta{Key: "wecom.websocket_url", Label: "Connection address", Short: "WeCom: address of the live connection; empty uses WeCom's own.", Group: GroupChatApps, Order: 2400, Advanced: true, Apply: Restart}})

// WeCom app: company ID
var WecomCallbackCorpID = Register(&String{Meta: Meta{Key: "wecom_callback.corp_id", Label: "Company ID", Short: "WeCom app: ID of your company in WeCom.", Group: GroupChatApps, Order: 2410, Advanced: true, Apply: Restart}})

// WeCom app: agent ID of the app
var WecomCallbackAgentID = Register(&String{Meta: Meta{Key: "wecom_callback.agent_id", Label: "App ID", Short: "WeCom app: agent ID of the self-built app.", Group: GroupChatApps, Order: 2410, Advanced: true, Apply: Restart}})

// WeCom app: address the callback server listens on
var WecomCallbackHost = Register(&String{Meta: Meta{Key: "wecom_callback.host", Label: "Listen address", Short: "WeCom app: address the message receiver listens on.", Group: GroupChatApps, Order: 2410, Advanced: true, Apply: Restart}})

// WeCom app: port the callback server listens on
var WecomCallbackPort = Register(&Int{Meta: Meta{Key: "wecom_callback.port", Label: "Listen port", Short: "WeCom app: port the message receiver listens on.", Group: GroupChatApps, Order: 2410, Advanced: true, Apply: Restart}, Default: 8645, Min: at(1), Max: at(65535)})

// WhatsApp Business: phone number ID
var WhatsappCloudPhoneNumberID = Register(&String{Meta: Meta{Key: "whatsapp_cloud.phone_number_id", Label: "Phone number ID", Short: "WhatsApp Business: phone number ID from the Meta app dashboard.", Group: GroupChatApps, Order: 2360, Advanced: true, Apply: Restart}})

// WhatsApp Business: app ID
var WhatsappCloudAppID = Register(&String{Meta: Meta{Key: "whatsapp_cloud.app_id", Label: "App ID", Short: "WhatsApp Business: ID of your Meta app.", Group: GroupChatApps, Order: 2360, Advanced: true, Apply: Restart}})

// WhatsApp Business: business account ID
var WhatsappCloudWabaID = Register(&String{Meta: Meta{Key: "whatsapp_cloud.waba_id", Label: "Business account", Short: "WhatsApp Business: ID of the business account.", Group: GroupChatApps, Order: 2360, Advanced: true, Apply: Restart}})

// WhatsApp Business: address the webhook receiver listens on
var WhatsappCloudWebhookHost = Register(&String{Meta: Meta{Key: "whatsapp_cloud.webhook_host", Label: "Listen address", Short: "WhatsApp Business: address the message receiver listens on.", Group: GroupChatApps, Order: 2360, Advanced: true, Apply: Restart}})

// WhatsApp Business: port the webhook receiver listens on
var WhatsappCloudWebhookPort = Register(&Int{Meta: Meta{Key: "whatsapp_cloud.webhook_port", Label: "Listen port", Short: "WhatsApp Business: port the message receiver listens on.", Group: GroupChatApps, Order: 2360, Advanced: true, Apply: Restart}, Min: at(1), Max: at(65535), Null: "the built-in port"})

// WhatsApp Business: path the webhook receiver answers on
var WhatsappCloudWebhookPath = Register(&String{Meta: Meta{Key: "whatsapp_cloud.webhook_path", Label: "Webhook path", Short: "WhatsApp Business: path the message receiver answers on.", Group: GroupChatApps, Order: 2360, Advanced: true, Apply: Restart}})

// WhatsApp Business: Graph API version
var WhatsappCloudAPIVersion = Register(&String{Meta: Meta{Key: "whatsapp_cloud.api_version", Label: "API version", Short: "WhatsApp Business: version of Meta's Graph API to call.", Group: GroupChatApps, Order: 2360, Advanced: true, Apply: Restart}})

// Slack: only answer channels after a fresh mention, never on thread context alone
var SlackStrictMention = Register(&Bool{Meta: Meta{Key: "slack.strict_mention", Label: "Strict mentions", Short: "Slack: answer in channels only when mentioned in that very message.", Group: GroupChatApps, Order: 3000, Advanced: true}, Default: false})

// Slack: refuse direct messages
var SlackDisableDms = Register(&Bool{Meta: Meta{Key: "slack.disable_dms", Label: "Refuse direct messages", Short: "Slack: ignore direct messages and answer only in channels.", Group: GroupChatApps, Order: 3010, Advanced: true}, Default: false})

// Slack: let unaddressed thread follow ups go unanswered
var SlackThreadFollowupSilence = Register(&Bool{Meta: Meta{Key: "slack.thread_followup_silence", Label: "Quiet thread follow-ups", Short: "Slack: stay silent on thread replies not addressed to it.", Group: GroupChatApps, Order: 3020, Advanced: true}, Default: false})

// Slack: channels where messages are left to the channel's own tooling
var SlackIgnoredChannels = Register(&List{Meta: Meta{Key: "slack.ignored_channels", Label: "Ignored channels", Short: "Slack: channel IDs it never answers in.", Group: GroupChatApps, Order: 3030, Advanced: true}})

// Slack: reactions that mark a message as being worked on
var SlackReactions = Register(&Bool{Meta: Meta{Key: "slack.reactions", Label: "Progress reactions", Short: "Slack: react to a message while working on it and when done.", Group: GroupChatApps, Order: 3040, Advanced: true}, Default: true})

// Slack: emoji reactions that hand a message to the agent
var SlackReactionTriggers = Register(&List{Meta: Meta{Key: "slack.reaction_triggers", Label: "Reaction triggers", Short: "Slack: emoji that, added to a message, hand it to the agent.", Help: "All, or a star, takes every emoji.", Group: GroupChatApps, Order: 3050, Advanced: true}, Null: "reactions hand nothing over"})

// Slack: channel a reaction hands its message to
var SlackReactionTriggerTarget = Register(&String{Meta: Meta{Key: "slack.reaction_trigger_target", Label: "Reaction target", Short: "Slack: channel, or channel:thread, that reaction hand-offs go to.", Group: GroupChatApps, Order: 3060, Advanced: true}, Null: "the channel the reaction was added in"})

// Mattermost: address of the Mattermost server
var MattermostURL = Register(&String{Meta: Meta{Key: "mattermost.url", Label: "Server address", Short: "Mattermost: address of your Mattermost server.", Help: "For example https://chat.example.com. The bot token stays with your other secrets.", Group: GroupChatApps, Order: 3100, Advanced: true, Apply: Restart}, Null: "not connected"})

// DingTalk: only answer group chats when triggered
var DingtalkRequireMention = Register(&Bool{Meta: Meta{Key: "dingtalk.require_mention", Label: "Require mention", Short: "DingTalk: only answer in group chats when mentioned.", Group: GroupChatApps, Order: 3200, Advanced: true}, Default: false})

// DingTalk: words that count as calling the bot
var DingtalkMentionPatterns = Register(&List{Meta: Meta{Key: "dingtalk.mention_patterns", Label: "Wake patterns", Short: "DingTalk: patterns in a message that count as mentioning it.", Help: "Regular expressions, matched without regard to case.", Group: GroupChatApps, Order: 3210, Advanced: true, Apply: Restart}})

// DingTalk: group chats answered without a mention
var DingtalkFreeResponseChats = Register(&List{Meta: Meta{Key: "dingtalk.free_response_chats", Label: "Free reply chats", Short: "DingTalk: group chat IDs where it replies without a mention.", Group: GroupChatApps, Order: 3220, Advanced: true}})

// DingTalk: the only group chats it answers in
var DingtalkAllowedChats = Register(&List{Meta: Meta{Key: "dingtalk.allowed_chats", Label: "Allowed chats", Short: "DingTalk: only answer in these group chat IDs.", Help: "Empty allows every group chat.", Group: GroupChatApps, Order: 3230, Advanced: true}})

// Feishu: which of the two clouds the app lives in
var FeishuDomain = Register(&Enum{Meta: Meta{Key: "feishu.domain", Label: "Cloud", Short: "Feishu: whether the app is on Feishu in China or on Lark.", Group: GroupChatApps, Order: 3300, Advanced: true, Apply: Restart}, Default: "feishu", Values: []Choice{{Value: "feishu", Label: "Feishu", Meaning: "open.feishu.cn"}, {Value: "lark", Label: "Lark", Meaning: "open.larksuite.com"}}})

// Feishu: how messages reach icebath
var FeishuConnectionMode = Register(&Enum{Meta: Meta{Key: "feishu.connection_mode", Label: "Connection", Short: "Feishu: receive messages over a socket or through a web address.", Group: GroupChatApps, Order: 3310, Advanced: true, Apply: Restart}, Default: "websocket", Values: []Choice{{Value: "websocket", Label: "Socket", Meaning: "No public address needed."}, {Value: "webhook", Label: "Web address", Meaning: "Feishu calls an address you expose."}}})

// BlueBubbles: interface the message listener binds to
var BluebubblesWebhookHost = Register(&String{Meta: Meta{Key: "bluebubbles.webhook_host", Label: "Listener address", Short: "BlueBubbles: address the listener for new messages binds to.", Group: GroupChatApps, Order: 3400, Advanced: true, Apply: Restart}, Default: "127.0.0.1"})

// BlueBubbles: port the message listener binds to
var BluebubblesWebhookPort = Register(&Int{Meta: Meta{Key: "bluebubbles.webhook_port", Label: "Listener port", Short: "BlueBubbles: port the listener for new messages binds to.", Group: GroupChatApps, Order: 3410, Advanced: true, Apply: Restart}, Default: 8645, Min: at(1), Max: at(65535)})

// BlueBubbles: path the server posts new messages to
var BluebubblesWebhookPath = Register(&String{Meta: Meta{Key: "bluebubbles.webhook_path", Label: "Listener path", Short: "BlueBubbles: path the server sends new messages to.", Group: GroupChatApps, Order: 3420, Advanced: true, Apply: Restart}, Default: "/bluebubbles-webhook"})

// BlueBubbles: read receipts on handled messages
var BluebubblesSendReadReceipts = Register(&Bool{Meta: Meta{Key: "bluebubbles.send_read_receipts", Label: "Read receipts", Short: "BlueBubbles: mark messages as read once they are handled.", Group: GroupChatApps, Order: 3430, Advanced: true, Apply: Restart}, Default: true})

// BlueBubbles: group chats answered only when called
var BluebubblesRequireMention = Register(&Bool{Meta: Meta{Key: "bluebubbles.require_mention", Label: "Require mention", Short: "BlueBubbles: only answer in group chats when called by name.", Group: GroupChatApps, Order: 3440, Advanced: true, Apply: Restart}, Default: false})

// BlueBubbles: words that count as calling the bot in a group
var BluebubblesMentionPatterns = Register(&List{Meta: Meta{Key: "bluebubbles.mention_patterns", Label: "Wake patterns", Short: "BlueBubbles: patterns in a group message that count as calling it.", Help: "Regular expressions, matched without regard to case.", Group: GroupChatApps, Order: 3450, Advanced: true, Apply: Restart}, Null: "the built-in patterns"})

// Weixin: address of the iLink service the account talks to
var WeixinBaseURL = Register(&String{Meta: Meta{Key: "weixin.base_url", Label: "Service address", Short: "Weixin: address of the iLink service the account talks to.", Help: "Signing in by QR code sets it.", Group: GroupChatApps, Order: 3500, Advanced: true, Apply: Restart}, Default: "https://ilinkai.weixin.qq.com"})

// Weixin: address media files are fetched from
var WeixinCdnBaseURL = Register(&String{Meta: Meta{Key: "weixin.cdn_base_url", Label: "Media address", Short: "Weixin: address media files are downloaded from.", Group: GroupChatApps, Order: 3510, Advanced: true, Apply: Restart}, Default: "https://novac2c.cdn.weixin.qq.com/c2c"})

// Weixin: each line of a reply sent as its own message
var WeixinSplitMultilineMessages = Register(&Bool{Meta: Meta{Key: "weixin.split_multiline_messages", Label: "Split lines", Short: "Weixin: send each line of a reply as its own message.", Group: GroupChatApps, Order: 3520, Advanced: true, Apply: Restart}, Default: false})

// Signal: story posts skipped
var SignalIgnoreStories = Register(&Bool{Meta: Meta{Key: "signal.ignore_stories", Label: "Skip stories", Short: "Signal: ignore story posts from contacts.", Group: GroupChatApps, Order: 3800, Advanced: true, Apply: Restart}, Default: true})

// Signal: group chats answered only when mentioned
var SignalRequireMention = Register(&Bool{Meta: Meta{Key: "signal.require_mention", Label: "Require mention", Short: "Signal: only answer in group chats when mentioned.", Group: GroupChatApps, Order: 3810, Advanced: true, Apply: Restart}, Default: false})

// Yuanbao: socket address the bot connects to
var YuanbaoWsURL = Register(&String{Meta: Meta{Key: "yuanbao.ws_url", Label: "Socket address", Short: "Yuanbao: address of the socket the bot receives messages on.", Group: GroupChatApps, Order: 3900, Advanced: true, Apply: Restart}, Default: "wss://bot-wss.yuanbao.tencent.com/wss/connection"})

// Yuanbao: address of the bot service
var YuanbaoAPIDomain = Register(&String{Meta: Meta{Key: "yuanbao.api_domain", Label: "Service address", Short: "Yuanbao: address of the bot service that signs the bot in.", Group: GroupChatApps, Order: 3910, Advanced: true, Apply: Restart}, Default: "https://bot.yuanbao.tencent.com"})

// Yuanbao: service environment requests are routed to
var YuanbaoRouteEnv = Register(&String{Meta: Meta{Key: "yuanbao.route_env", Label: "Service route", Short: "Yuanbao: which service environment the bot's requests go to.", Group: GroupChatApps, Order: 3920, Advanced: true, Apply: Restart}, Null: "the production service"})

// Slack: messages from other bots
var SlackAllowBots = Register(&Enum{Meta: Meta{Key: "slack.allow_bots", Label: "Other bots", Short: "Slack: whether messages from other bots are answered.", Group: GroupChatApps, Order: 3070, Advanced: true}, Default: "none", Values: []Choice{{Value: "none", Label: "Never", Meaning: "Ignore other bots."}, {Value: "mentions", Label: "When mentioned", Meaning: "Answer bots that mention it."}, {Value: "all", Label: "Always", Meaning: "Answer every bot message."}}})

// DingTalk: users allowed to use the bot
var DingtalkAllowedUsers = Register(&List{Meta: Meta{Key: "dingtalk.allowed_users", Label: "Allowed users", Short: "DingTalk: user or staff IDs allowed to use the bot.", Help: "Empty or a star allows everyone.", Group: GroupChatApps, Order: 3240, Advanced: true, Apply: Restart}})

// Feishu: messages from other bots
var FeishuAllowBots = Register(&Enum{Meta: Meta{Key: "feishu.allow_bots", Label: "Other bots", Short: "Feishu: whether messages from other bots are answered.", Group: GroupChatApps, Order: 3320, Advanced: true, Apply: Restart}, Default: "none", Values: []Choice{{Value: "none", Label: "Never", Meaning: "Ignore other bots."}, {Value: "mentions", Label: "When mentioned", Meaning: "Answer bots that mention it."}, {Value: "all", Label: "Always", Meaning: "Answer every bot message."}}})

// Weixin: who may message the bot directly
var WeixinDmPolicy = Register(&Enum{Meta: Meta{Key: "weixin.dm_policy", Label: "Direct messages", Short: "Weixin: who may message the bot directly.", Group: GroupChatApps, Order: 3530, Advanced: true, Apply: Restart}, Default: "pairing", Open: true, Values: chatPolicyChoices})

// Weixin: people allowed to message the bot directly
var WeixinAllowFrom = Register(&List{Meta: Meta{Key: "weixin.allow_from", Label: "Allowed people", Short: "Weixin: user IDs allowed to message the bot directly.", Group: GroupChatApps, Order: 3540, Advanced: true, Apply: Restart}})

// Weixin: which group chats the bot answers in
var WeixinGroupPolicy = Register(&Enum{Meta: Meta{Key: "weixin.group_policy", Label: "Group chats", Short: "Weixin: which group chats the bot answers in.", Group: GroupChatApps, Order: 3550, Advanced: true, Apply: Restart}, Default: "disabled", Open: true, Values: chatPolicyChoices})

// Weixin: group chats the bot answers in
var WeixinGroupAllowFrom = Register(&List{Meta: Meta{Key: "weixin.group_allow_from", Label: "Allowed groups", Short: "Weixin: group chat IDs the bot answers in.", Group: GroupChatApps, Order: 3560, Advanced: true, Apply: Restart}})

// QQ: people allowed to message the bot directly
var QqbotAllowFrom = Register(&List{Meta: Meta{Key: "qqbot.allow_from", Label: "Allowed people", Short: "QQ: user OpenIDs allowed to message the bot directly.", Group: GroupChatApps, Order: 3600, Advanced: true, Apply: Restart}})

// QQ: group chats the bot answers in
var QqbotGroupAllowFrom = Register(&List{Meta: Meta{Key: "qqbot.group_allow_from", Label: "Allowed groups", Short: "QQ: group OpenIDs the bot answers in.", Group: GroupChatApps, Order: 3610, Advanced: true, Apply: Restart}})

// Yuanbao: who may message the bot directly
var YuanbaoDmPolicy = Register(&Enum{Meta: Meta{Key: "yuanbao.dm_policy", Label: "Direct messages", Short: "Yuanbao: who may message the bot directly.", Group: GroupChatApps, Order: 3930, Advanced: true, Apply: Restart}, Default: "pairing", Open: true, Values: chatPolicyChoices})

// Yuanbao: people allowed to message the bot directly
var YuanbaoDmAllowFrom = Register(&List{Meta: Meta{Key: "yuanbao.dm_allow_from", Label: "Allowed people", Short: "Yuanbao: user IDs allowed to message the bot directly.", Group: GroupChatApps, Order: 3940, Advanced: true, Apply: Restart}})

// Yuanbao: which group chats the bot answers in
var YuanbaoGroupPolicy = Register(&Enum{Meta: Meta{Key: "yuanbao.group_policy", Label: "Group chats", Short: "Yuanbao: which group chats the bot answers in.", Group: GroupChatApps, Order: 3950, Advanced: true, Apply: Restart}, Default: "pairing", Open: true, Values: chatPolicyChoices})

// Yuanbao: group chats the bot answers in
var YuanbaoGroupAllowFrom = Register(&List{Meta: Meta{Key: "yuanbao.group_allow_from", Label: "Allowed groups", Short: "Yuanbao: group IDs the bot answers in.", Group: GroupChatApps, Order: 3960, Advanced: true, Apply: Restart}})

// Chat app connections the background service starts, by app
var Platforms = Register(&Map{Meta: Meta{Key: "platforms", Label: "Chat app connections", Short: "Each chat app's connection: on or off, tokens and options.", Group: GroupChatApps, Order: 3990, Advanced: true, Apply: Restart}})

// One conversation per person in group chats
var GatewayGroupSessionsPerUser = Register(&Bool{Meta: Meta{Key: "gateway.group_sessions_per_user", Label: "Group per person", Short: "In group chats, keep a separate conversation per person.", Group: GroupChatApps, Order: 3991, Advanced: true, Apply: Restart}, Default: true})

// One conversation per person in threads
var GatewayThreadSessionsPerUser = Register(&Bool{Meta: Meta{Key: "gateway.thread_sessions_per_user", Label: "Thread per person", Short: "In threads, keep a separate conversation per person.", Group: GroupChatApps, Order: 3992, Advanced: true, Apply: Restart}})

// Keeps a local log of chat app messages
var GatewayAlwaysLogLocal = Register(&Bool{Meta: Meta{Key: "gateway.always_log_local", Label: "Keep local log", Short: "Keep a local copy of chat app conversations.", Group: GroupChatApps, Order: 3993, Advanced: true, Apply: Restart}, Default: true})

// Drops replies that only narrate silence
var GatewayFilterSilenceNarration = Register(&Bool{Meta: Meta{Key: "gateway.filter_silence_narration", Label: "Skip silence notes", Short: "Do not send replies that only say nothing happened.", Group: GroupChatApps, Order: 3994, Advanced: true, Apply: Restart}, Default: true})

// Messages that start a new conversation
var GatewayResetTriggers = Register(&List{Meta: Meta{Key: "gateway.reset_triggers", Label: "Reset words", Short: "Messages that start a new conversation in a chat app.", Group: GroupChatApps, Order: 3997, Advanced: true, Apply: Restart}, Default: []any{"/new", "/reset"}})

// What happens when a stranger writes to the bot
var GatewayUnauthorizedDMBehavior = Register(&Enum{Meta: Meta{Key: "gateway.unauthorized_dm_behavior", Label: "Strangers", Short: "What the bot does when someone it does not know writes.", Group: GroupChatApps, Order: 3998, Advanced: true, Apply: Restart}, Default: "pair", Values: []Choice{{Value: "pair", Label: "Offer pairing", Meaning: "Reply with a pairing code."}, {Value: "ignore", Label: "Ignore", Meaning: "Do not reply."}, {Value: "decline", Label: "Decline", Meaning: "Reply with the decline message."}}})

// Reply sent to strangers when they are declined
var GatewayUnauthorizedDMDeclineMessage = Register(&String{Meta: Meta{Key: "gateway.unauthorized_dm_decline_message", Label: "Decline message", Short: "Reply sent to strangers when Strangers is Decline.", Group: GroupChatApps, Order: 3999, Advanced: true, Apply: Restart}})
