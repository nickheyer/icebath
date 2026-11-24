package settings

import "time"

// Let scheduled jobs create and change other scheduled jobs
var CronAllowAgentScheduling = Register(&Bool{Meta: Meta{Key: "cron.allow_agent_scheduling", Label: "Agent scheduling", Short: "Let scheduled jobs create and change other scheduled jobs.", Group: GroupChatApps, Order: 1000, Advanced: true}, Default: false})

// Run jobs missed while the app was off, once when it returns
var CronCatchUpMissed = Register(&Bool{Meta: Meta{Key: "cron.catch_up_missed", Label: "Catch up missed", Short: "Run jobs missed while the app was off, once when it returns.", Group: GroupChatApps, Order: 1010, Advanced: true}, Default: true})

// How long to wait for cleanup after a scheduled job finishes
var CronCleanupTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "cron.cleanup_timeout_seconds", Label: "Cleanup wait", Short: "How long to wait for cleanup after a scheduled job finishes.", Help: "Cleanup that runs longer is abandoned.", Group: GroupChatApps, Order: 1020, Advanced: true}, Default: 10 * time.Second, Min: at(time.Duration(0))})

// Days a finished one-time job stays in the list before it is removed
var CronCompletedRetentionDays = Register(&Float{Meta: Meta{Key: "cron.completed_retention_days", Label: "Done job retention", Short: "Days a finished one-time job stays in the list before it is removed.", Help: "Zero keeps them.", Group: GroupChatApps, Order: 1025, Advanced: true}, Default: 7, Min: at(0.0), Unit: "days"})

// Send a notice to the chat when a scheduled result is delivered
var CronDeliveryNotify = Register(&Bool{Meta: Meta{Key: "cron.delivery.notify", Label: "Delivery notices", Short: "Send a notice to the chat when a scheduled result is delivered.", Group: GroupChatApps, Order: 1030, Advanced: true}, Default: true})

// Failed runs in a row before you get a nudge about a job
var CronFailureNudgeThreshold = Register(&Int{Meta: Meta{Key: "cron.failure_nudge_threshold", Label: "Failure nudge after", Short: "Failed runs in a row before you get a nudge about a job.", Group: GroupChatApps, Order: 1040, Advanced: true}, Default: 3, Unit: "failures"})

// Wait between repeat alerts for a job that keeps failing
var CronFailureRepeatAlertHours = Register(&Duration{Meta: Meta{Key: "cron.failure_repeat_alert_hours", Label: "Repeat alert gap", Short: "Wait between repeat alerts for a job that keeps failing.", Help: "Zero alerts on every failed run.", Group: GroupChatApps, Order: 1050, Advanced: true}, Default: 6 * time.Hour, Unit: time.Hour})

// How long a job may go without activity before it is stopped
var CronInactivityTimeout = Register(&Duration{Meta: Meta{Key: "cron.inactivity_timeout", Label: "Job idle limit", Short: "How long a job may go without activity before it is stopped.", Help: "Zero never stops it.", Group: GroupChatApps, Order: 1055, Advanced: true, Env: "ICEBATH_CRON_TIMEOUT"}, Default: 10 * time.Minute, Also: map[string]time.Duration{"inf": 0, "infinity": 0, "never": 0, "unlimited": 0}})

// Minimum time a running job gets before it counts as stuck
var CronInflightMaxMinutes = Register(&Duration{Meta: Meta{Key: "cron.inflight_max_minutes", Label: "Stuck job limit", Short: "Minimum time a running job gets before it counts as stuck.", Help: "The matching environment variable wins.", Group: GroupChatApps, Order: 1060, Advanced: true, Env: "ICEBATH_CRON_INFLIGHT_MAX_MINUTES"}, Unit: time.Minute, Null: "30 minutes"})

// Most scheduled jobs that run at the same time
var CronMaxParallelJobs = Register(&Int{Meta: Meta{Key: "cron.max_parallel_jobs", Label: "Parallel jobs", Short: "Most scheduled jobs that run at the same time.", Help: "Zero also means no limit. The matching environment variable wins.", Group: GroupChatApps, Order: 1070, Advanced: true, Env: "ICEBATH_CRON_MAX_PARALLEL"}, Unit: "jobs", Min: at(0), Null: "no limit"})

// How long to wait when sending each file from a scheduled job
var CronMediaSendTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "cron.media_send_timeout_seconds", Label: "Media send wait", Short: "How long to wait when sending each file from a scheduled job.", Group: GroupChatApps, Order: 1080, Advanced: true, Env: "ICEBATH_CRON_MEDIA_SEND_TIMEOUT"}, Default: 5 * time.Minute})

// Also add scheduled results to the chat session they were sent to
var CronMirrorDelivery = Register(&Bool{Meta: Meta{Key: "cron.mirror_delivery", Label: "Mirror results", Short: "Also add scheduled results to the chat session they were sent to.", Help: "A job can override this.", Group: GroupChatApps, Order: 1090, Advanced: true}, Default: false})

// How late a scheduled job may still start after its time
var CronMisfireGraceMinutes = Register(&Duration{Meta: Meta{Key: "cron.misfire_grace_minutes", Label: "Late start allowance", Short: "How late a scheduled job may still start after its time.", Group: GroupChatApps, Order: 1100, Advanced: true}, Default: 10 * time.Minute, Unit: time.Minute})

// Model scheduled jobs use when they do not pick their own
var CronModel = Register(&String{Meta: Meta{Key: "cron.model", Label: "Job model", Short: "Model scheduled jobs use when they do not pick their own.", Help: "Blank follows your main model. The matching environment variable wins.", Group: GroupChatApps, Order: 1110, Advanced: true, Env: "ICEBATH_MODEL"}, Default: ""})

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
var CronScriptTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "cron.script_timeout_seconds", Label: "Job script limit", Short: "Longest a job pre-run script may run.", Group: GroupChatApps, Order: 1180, Advanced: true, Env: "ICEBATH_CRON_SCRIPT_TIMEOUT"}, Default: time.Hour})

// How long a job waits to open the session history
var CronSessionDbTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "cron.session_db_timeout_seconds", Label: "History open wait", Short: "How long a job waits to open the session history.", Help: "Zero waits forever. The matching environment variable wins.", Group: GroupChatApps, Order: 1190, Advanced: true, Env: "ICEBATH_CRON_SESSION_DB_TIMEOUT"}, Default: 10 * time.Second})

// How long to wait when a job sends outside a chat session
var CronStandaloneSendTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "cron.standalone_send_timeout_seconds", Label: "Direct send wait", Short: "How long to wait when a job sends outside a chat session.", Help: "Zero or less uses the default.", Group: GroupChatApps, Order: 1200, Advanced: true}, Default: time.Minute})

// Add a header and footer around scheduled results sent to chat
var CronWrapResponse = Register(&Bool{Meta: Meta{Key: "cron.wrap_response", Label: "Wrap results", Short: "Add a header and footer around scheduled results sent to chat.", Group: GroupChatApps, Order: 1210, Advanced: true}, Default: true})

// Discord: only answer in these channel IDs
var DiscordAllowedChannels = Register(&List{Meta: Meta{Key: "discord.allowed_channels", Label: "Allowed channels", Short: "Discord: only answer in these channel IDs.", Group: GroupChatApps, Order: 1220, Apply: Live, Advanced: true, Env: "DISCORD_ALLOWED_CHANNELS"}})

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
var DiscordServerActions = Register(&List{Meta: Meta{Key: "discord.server_actions", Label: "Allowed server actions", Short: "Discord: server actions the agent may take.", Help: "Comma separated action names. Empty allows all, and unknown names are ignored.", Group: GroupChatApps, Order: 1410, Advanced: true}, Null: "every action"})

// Discord: also require a mention inside threads
var DiscordThreadRequireMention = Register(&Bool{Meta: Meta{Key: "discord.thread_require_mention", Label: "Threads need mention", Short: "Discord: also require a mention inside threads.", Group: GroupChatApps, Order: 1420, Apply: Live, Advanced: true}, Default: false})

// Discord: leave a voice channel after this much silence
var DiscordVoiceChannelInactivityTimeoutSeconds = Register(&Duration{Meta: Meta{Key: "discord.voice_channel_inactivity_timeout_seconds", Label: "Voice idle leave", Short: "Discord: leave a voice channel after this much silence.", Help: "Zero never leaves.", Group: GroupChatApps, Order: 1430, Apply: Restart, Advanced: true}, Default: 5 * time.Minute})

// Discord: say a short phrase when starting on a voice request
var DiscordVoiceFxAckEnabled = Register(&Bool{Meta: Meta{Key: "discord.voice_fx.ack_enabled", Label: "Spoken acknowledgements", Short: "Discord: say a short phrase when starting on a voice request.", Group: GroupChatApps, Order: 1440, Apply: Restart, Advanced: true}, Default: true})

// Discord: phrases spoken while it works on a voice request
var DiscordVoiceFxAckPhrases = Register(&List{Meta: Meta{Key: "discord.voice_fx.ack_phrases", Label: "Acknowledgement phrases", Short: "Discord: phrases spoken while it works on a voice request.", Group: GroupChatApps, Order: 1450, Apply: Restart, Advanced: true}, Null: "the built-in phrases in your language"})

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
var DiscordVoiceFxLeadSilenceMs = Register(&Duration{Meta: Meta{Key: "discord.voice_fx.lead_silence_ms", Label: "Speech lead silence", Short: "Discord: silence added before speech so the start is not clipped.", Group: GroupChatApps, Order: 1510, Apply: Restart, Advanced: true}, Default: 200 * time.Millisecond, Unit: time.Millisecond})

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

// Discord: wait for more text before answering a message sent in parts
var DiscordTextBatchDelaySeconds = Register(&Duration{Meta: Meta{Key: "discord.text_batch_delay_seconds", Label: "Text gather wait", Short: "Discord: wait for more text before answering a message sent in parts.", Group: GroupChatApps, Order: 1590, Apply: Restart, Advanced: true, Env: "ICEBATH_DISCORD_TEXT_BATCH_DELAY_SECONDS"}, Default: 600 * time.Millisecond})

// Discord: longer wait when a long message arrives split by Discord
var DiscordTextBatchSplitDelaySeconds = Register(&Duration{Meta: Meta{Key: "discord.text_batch_split_delay_seconds", Label: "Split text wait", Short: "Discord: longer wait when a long message arrives split by Discord.", Group: GroupChatApps, Order: 1591, Apply: Restart, Advanced: true, Env: "ICEBATH_DISCORD_TEXT_BATCH_SPLIT_DELAY_SECONDS"}, Default: 2 * time.Second})

// Discord: offer the slash commands
var DiscordSlashCommands = Register(&Bool{Meta: Meta{Key: "discord.slash_commands", Label: "Slash commands", Short: "Discord: offer the slash commands.", Group: GroupChatApps, Order: 1592, Apply: Restart, Advanced: true}, Default: true})

// Discord: show the slash commands only to server admins
var DiscordHideSlashCommands = Register(&Bool{Meta: Meta{Key: "discord.hide_slash_commands", Label: "Admin-only commands", Short: "Discord: show the slash commands only to server admins.", Group: GroupChatApps, Order: 1593, Apply: Restart, Advanced: true, Env: "DISCORD_HIDE_SLASH_COMMANDS"}, Default: false})

// Discord: how slash commands are synced with Discord
var DiscordCommandSyncPolicy = Register(&Enum{Meta: Meta{Key: "discord.command_sync_policy", Label: "Command sync", Short: "Discord: how slash commands are synced with Discord.", Group: GroupChatApps, Order: 1594, Apply: Restart, Advanced: true, Env: "DISCORD_COMMAND_SYNC_POLICY"}, Default: "safe", Values: []Choice{{Value: "safe", Label: "Safe", Meaning: "Change only the commands that differ."}, {Value: "bulk", Label: "Bulk", Meaning: "Replace every command at once."}, {Value: "off", Label: "Off", Meaning: "Leave the commands as they are."}}})

// Discord: skip messages that mention other people but not it
var DiscordIgnoreNoMention = Register(&Bool{Meta: Meta{Key: "discord.ignore_no_mention", Label: "Skip others' mentions", Short: "Discord: skip messages that mention other people but not it.", Help: "Bot threads and free reply channels are exempt.", Group: GroupChatApps, Order: 1595, Apply: Live, Advanced: true, Env: "DISCORD_IGNORE_NO_MENTION"}, Default: true})

// Discord: let replies ping @everyone and @here
var DiscordAllowMentionsEveryone = Register(&Bool{Meta: Meta{Key: "discord.allow_mentions.everyone", Label: "Ping everyone", Short: "Discord: let replies ping @everyone and @here.", Group: GroupChatApps, Order: 1596, Apply: Restart, Advanced: true, Env: "DISCORD_ALLOW_MENTION_EVERYONE"}, Default: false})

// Discord: let replies ping roles
var DiscordAllowMentionsRoles = Register(&Bool{Meta: Meta{Key: "discord.allow_mentions.roles", Label: "Ping roles", Short: "Discord: let replies ping roles.", Group: GroupChatApps, Order: 1597, Apply: Restart, Advanced: true, Env: "DISCORD_ALLOW_MENTION_ROLES"}, Default: false})

// Discord: let replies ping people they name
var DiscordAllowMentionsUsers = Register(&Bool{Meta: Meta{Key: "discord.allow_mentions.users", Label: "Ping people", Short: "Discord: let replies ping people they name.", Group: GroupChatApps, Order: 1598, Apply: Restart, Advanced: true, Env: "DISCORD_ALLOW_MENTION_USERS"}, Default: true})

// Discord: let a reply ping the person it answers
var DiscordAllowMentionsRepliedUser = Register(&Bool{Meta: Meta{Key: "discord.allow_mentions.replied_user", Label: "Ping on reply", Short: "Discord: let a reply ping the person it answers.", Group: GroupChatApps, Order: 1599, Apply: Restart, Advanced: true, Env: "DISCORD_ALLOW_MENTION_REPLIED_USER"}, Default: true})

// Discord: ffmpeg program voice playback and recording run
var DiscordFfmpegPath = Register(&String{Meta: Meta{Key: "discord.ffmpeg_path", Label: "ffmpeg program", Short: "Discord: ffmpeg program voice playback and recording run.", Help: "Blank finds ffmpeg on this machine.", Group: GroupChatApps, Order: 1600, Apply: Restart, Advanced: true, Env: "FFMPEG_PATH"}, Default: ""})

// Details shown in the footer line under chat replies
var DisplayRuntimeFooterFields = Register(&List{Meta: Meta{Key: "display.runtime_footer.fields", Label: "Footer fields", Short: "Details shown in the footer line under chat replies.", Help: "Choose from model, served_model, context_pct, latency and cwd, in order.", Group: GroupChatApps, Order: 1630, Advanced: true, Platforms: true}, Default: []any{"model", "context_pct", "cwd"}})

// Let anyone message the chat apps, without an allowed list
var GatewayAllowAllUsers = Register(&Bool{Meta: Meta{Key: "gateway.allow_all_users", Label: "Allow all users", Short: "Let anyone message the chat apps, without an allowed list.", Group: GroupChatApps, Order: 1640, Apply: Live, Advanced: true, Env: "GATEWAY_ALLOW_ALL_USERS"}, Default: false})

// People allowed to message every chat app, by their user ID
var GatewayAllowedUsers = Register(&List{Meta: Meta{Key: "gateway.allowed_users", Label: "Allowed users", Short: "People allowed to message every chat app, by their user ID.", Group: GroupChatApps, Order: 1645, Apply: Live, Advanced: true, Env: "GATEWAY_ALLOWED_USERS"}})

// API server: longest tool output kept in saved conversation history
var GatewayApiServerHistoryToolOutputMaxChars = Register(&Int{Meta: Meta{Key: "gateway.api_server.history_tool_output_max_chars", Label: "API output cap", Short: "API server: longest tool output kept in saved conversation history.", Help: "Zero keeps full tool output.", Group: GroupChatApps, Order: 1650, Apply: Restart, Advanced: true}, Default: 0, Unit: "characters", Min: at(0)})

// API server: most requests answered at the same time
var GatewayApiServerMaxConcurrentRuns = Register(&Int{Meta: Meta{Key: "gateway.api_server.max_concurrent_runs", Label: "API parallel runs", Short: "API server: most requests answered at the same time.", Group: GroupChatApps, Order: 1660, Apply: Restart, Advanced: true}, Default: 10, Unit: "runs", Min: at(0)})

// Let the background service merge profiles into one when it can
var GatewayAutoMultiplexMigration = Register(&Bool{Meta: Meta{Key: "gateway.auto_multiplex_migration", Label: "Auto merge profiles", Short: "Let the background service merge profiles into one when it can.", Group: GroupChatApps, Order: 1670, Advanced: true}, Default: true})

// How long a chat is ignored after a bot-to-bot loop is caught
var GatewayBotLoopGuardCooldownSeconds = Register(&Duration{Meta: Meta{Key: "gateway.bot_loop_guard.cooldown_seconds", Label: "Bot loop pause", Short: "How long a chat is ignored after a bot-to-bot loop is caught.", Group: GroupChatApps, Order: 1680, Advanced: true}, Min: at(time.Second), Default: 10 * time.Minute, DefaultBelow: true})

// Stop replying when bots keep triggering each other in a chat
var GatewayBotLoopGuardEnabled = Register(&Bool{Meta: Meta{Key: "gateway.bot_loop_guard.enabled", Label: "Bot loop guard", Short: "Stop replying when bots keep triggering each other in a chat.", Group: GroupChatApps, Order: 1690, Advanced: true}, Default: true})

// Bot messages allowed in the window before the guard trips
var GatewayBotLoopGuardMaxEvents = Register(&Int{Meta: Meta{Key: "gateway.bot_loop_guard.max_events", Label: "Bot loop messages", Short: "Bot messages allowed in the window before the guard trips.", Group: GroupChatApps, Order: 1700, Advanced: true}, Default: 20, Unit: "messages", Min: at(1), DefaultBelow: true})

// Time span over which bot messages are counted
var GatewayBotLoopGuardWindowSeconds = Register(&Duration{Meta: Meta{Key: "gateway.bot_loop_guard.window_seconds", Label: "Bot loop window", Short: "Time span over which bot messages are counted.", Group: GroupChatApps, Order: 1710, Advanced: true}, Min: at(time.Second), Default: 5 * time.Minute, DefaultBelow: true})

// Run the background service under the container's supervisor
var GatewayContainerSupervision = Register(&Bool{Meta: Meta{Key: "gateway.container_supervision", Label: "Container supervision", Short: "In a container, runs the service under its supervisor so it restarts.", Help: "Setting the matching environment variable to on turns it off.", Group: GroupChatApps, Order: 1715, Apply: Restart, Advanced: true, Env: "ICEBATH_GATEWAY_NO_SUPERVISE", EnvInvert: true}, Default: true})

// Record sent replies so a restart does not lose or repeat them
var GatewayDeliveryLedger = Register(&Bool{Meta: Meta{Key: "gateway.delivery_ledger", Label: "Delivery records", Short: "Record sent replies so a restart does not lose or repeat them.", Group: GroupChatApps, Order: 1720, Advanced: true}, Default: true})

// Write a line for every way the background service exits
var GatewayExitRecords = Register(&Bool{Meta: Meta{Key: "gateway.exit_records", Label: "Exit records", Short: "Writes a line for every service exit to logs/gateway-exit-diag.log.", Group: GroupChatApps, Order: 1721, Advanced: true, Env: "ICEBATH_GATEWAY_EXIT_DIAG"}, Default: true})

// How long an idle chat app connection is kept open for reuse
var GatewayHTTPKeepaliveExpiry = Register(&Duration{Meta: Meta{Key: "gateway.http_keepalive_expiry", Label: "Chat keepalive time", Short: "How long an idle chat app connection is kept open for reuse.", Help: "Zero or below uses the default.", Group: GroupChatApps, Order: 1722, Apply: Restart, Advanced: true, Env: "ICEBATH_GATEWAY_HTTPX_KEEPALIVE_EXPIRY"}, Default: 2 * time.Second})

// Most idle connections each chat app keeps open for reuse
var GatewayHTTPMaxKeepalive = Register(&Int{Meta: Meta{Key: "gateway.http_max_keepalive", Label: "Chat idle connections", Short: "Most idle connections each chat app keeps open for reuse.", Group: GroupChatApps, Order: 1723, Apply: Restart, Advanced: true, Env: "ICEBATH_GATEWAY_HTTPX_MAX_KEEPALIVE"}, Default: 10, Min: at(1), Unit: "connections", DefaultBelow: true})

// Client ID for signing in to the relay's identity provider
var GatewayIdpClientID = Register(&String{Meta: Meta{Key: "gateway.idp.client_id", Label: "Relay sign-in client", Short: "Client ID for signing in to the relay's identity provider.", Group: GroupChatApps, Order: 1724, Apply: Restart, Advanced: true, Env: "GATEWAY_RELAY_IDP_CLIENT_ID"}})

// Client secret for signing in to the relay's identity provider
var GatewayIdpClientSecret = Register(&String{Meta: Meta{Key: "gateway.idp.client_secret", Label: "Relay sign-in secret", Short: "Client secret for signing in to the relay's identity provider.", Group: GroupChatApps, Order: 1725, Apply: Restart, Advanced: true, Env: "GATEWAY_RELAY_IDP_CLIENT_SECRET"}, Masked: true})

// Scope requested when signing in to the relay's identity provider
var GatewayIdpScope = Register(&String{Meta: Meta{Key: "gateway.idp.scope", Label: "Relay sign-in scope", Short: "Scope requested when signing in to the relay's identity provider.", Group: GroupChatApps, Order: 1726, Apply: Restart, Advanced: true, Env: "GATEWAY_RELAY_IDP_SCOPE"}})

// Token address of the identity provider the relay trusts
var GatewayIdpTokenURL = Register(&String{Meta: Meta{Key: "gateway.idp.token_url", Label: "Relay sign-in address", Short: "Token address of the identity provider the relay trusts.", Help: "Alone it names an endpoint that hands out tokens without a client.", Group: GroupChatApps, Order: 1727, Apply: Restart, Advanced: true, Env: "GATEWAY_RELAY_IDP_TOKEN_URL"}})

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

// How long each chat app gets to disconnect when the service stops or reconnects it
var GatewayAdapterDisconnectTimeout = Register(&Duration{Meta: Meta{Key: "gateway.adapter_disconnect_timeout", Label: "Chat disconnect wait", Short: "How long each chat app gets to disconnect when the service stops.", Help: "Zero stops without waiting.", Group: GroupChatApps, Order: 1825, Advanced: true, Env: "ICEBATH_GATEWAY_ADAPTER_DISCONNECT_TIMEOUT"}, Default: 5 * time.Second, Min: at(time.Duration(0))})

// Rules that send particular chats to a particular profile
var GatewayProfileRoutes = Register(&List{Meta: Meta{Key: "gateway.profile_routes", Label: "Profile routes", Short: "Rules that send particular chats to a particular profile.", Help: "Each entry names a chat and the profile that handles it.", Group: GroupChatApps, Order: 1830, Apply: Restart, Advanced: true}})

// Address of a remote icebath API server that answers the chats in place of this one
var GatewayProxyUrl = Register(&String{Meta: Meta{Key: "gateway.proxy_url", Label: "Remote agent", Short: "Address of an icebath API server that answers the chats instead.", Help: "Chat apps still connect from here and every turn runs on that server. Empty answers here.", Group: GroupChatApps, Order: 1840, Advanced: true, Env: "GATEWAY_PROXY_URL"}, Default: ""})

// Name the relay shows for this agent
var GatewayRelayDisplayName = Register(&String{Meta: Meta{Key: "gateway.relay_display_name", Label: "Relay agent name", Short: "Name the relay shows for this agent; blank uses the skin's agent name.", Group: GroupChatApps, Order: 1841, Apply: Restart, Advanced: true, Env: "GATEWAY_RELAY_DISPLAY_NAME"}})

// Public address the relay sends messages to
var GatewayRelayEndpoint = Register(&String{Meta: Meta{Key: "gateway.relay_endpoint", Label: "Relay inbound address", Short: "Public address the relay sends messages to; blank for outbound only.", Group: GroupChatApps, Order: 1842, Apply: Restart, Advanced: true, Env: "GATEWAY_RELAY_ENDPOINT"}})

// ID this background service enrolled with on the relay
var GatewayRelayID = Register(&String{Meta: Meta{Key: "gateway.relay_id", Label: "Relay service ID", Short: "ID this background service enrolled with on the relay.", Help: "Enrolling writes it to the profile's .env.", Group: GroupChatApps, Order: 1843, Apply: Restart, Advanced: true, Env: "GATEWAY_RELAY_ID"}})

// Stable instance ID sent to the relay when this service enrolls
var GatewayRelayInstanceID = Register(&String{Meta: Meta{Key: "gateway.relay_instance_id", Label: "Relay instance", Short: "Stable instance ID sent to the relay when this service enrolls.", Group: GroupChatApps, Order: 1844, Apply: Restart, Advanced: true, Env: "GATEWAY_RELAY_INSTANCE_ID"}})

// Chat apps the relay fronts for this service
var GatewayRelayPlatforms = Register(&List{Meta: Meta{Key: "gateway.relay_platforms", Label: "Relay chat apps", Short: "Chat apps the relay fronts for this service, such as discord.", Group: GroupChatApps, Order: 1845, Apply: Restart, Advanced: true, Env: "GATEWAY_RELAY_PLATFORMS"}})

// Route keys this service claims on the relay
var GatewayRelayRouteKeys = Register(&List{Meta: Meta{Key: "gateway.relay_route_keys", Label: "Relay routes", Short: "Route keys this service claims on the relay, such as server IDs.", Group: GroupChatApps, Order: 1846, Apply: Restart, Advanced: true, Env: "GATEWAY_RELAY_ROUTE_KEYS"}})

// Secret this background service proves itself with on the relay
var GatewayRelaySecret = Register(&String{Meta: Meta{Key: "gateway.relay_secret", Label: "Relay secret", Short: "Secret this background service proves itself with on the relay.", Help: "Enrolling writes it to the profile's .env.", Group: GroupChatApps, Order: 1847, Apply: Restart, Advanced: true, Env: "GATEWAY_RELAY_SECRET"}, Masked: true})

// Address of the relay connector this service dials
var GatewayRelayURL = Register(&String{Meta: Meta{Key: "gateway.relay_url", Label: "Relay address", Short: "Address of the relay connector this service dials for chat apps.", Group: GroupChatApps, Order: 1848, Apply: Restart, Advanced: true, Env: "GATEWAY_RELAY_URL"}})

// Address the relay calls to wake this machine after an idle suspend
var GatewayRelayWakeURL = Register(&String{Meta: Meta{Key: "gateway.relay_wake_url", Label: "Relay wake address", Short: "Address the relay calls to wake this machine after an idle suspend.", Group: GroupChatApps, Order: 1849, Apply: Restart, Advanced: true, Env: "GATEWAY_RELAY_WAKE_URL"}})

// Starts allowed in the window before startup is delayed
var GatewayRespawnStormMaxStarts = Register(&Int{Meta: Meta{Key: "gateway.respawn_storm.max_starts", Label: "Restart storm starts", Short: "Starts allowed in the window before startup is delayed.", Help: "Zero turns the check off. The matching environment variable wins.", Group: GroupChatApps, Order: 1850, Apply: Restart, Advanced: true, Env: "ICEBATH_GATEWAY_MAX_STARTS"}, Default: 5, Unit: "starts"})

// Time span over which service starts are counted
var GatewayRespawnStormWindowSeconds = Register(&Duration{Meta: Meta{Key: "gateway.respawn_storm.window_seconds", Label: "Restart storm window", Short: "Time span over which service starts are counted.", Help: "The matching environment variable wins.", Group: GroupChatApps, Order: 1860, Apply: Restart, Advanced: true, Env: "ICEBATH_GATEWAY_START_WINDOW_S"}, Default: 2 * time.Minute})

// Longest gap between restarts that still counts as a loop
var GatewayRestartLoopGuardMaxGapSeconds = Register(&Duration{Meta: Meta{Key: "gateway.restart_loop_guard.max_gap_seconds", Label: "Restart loop gap", Short: "Longest gap between restarts that still counts as a loop.", Group: GroupChatApps, Order: 1870, Advanced: true}, Default: 5 * time.Minute})

// Restarts in the window before the loop guard stops them
var GatewayRestartLoopGuardMaxRestarts = Register(&Int{Meta: Meta{Key: "gateway.restart_loop_guard.max_restarts", Label: "Restart loop count", Short: "Restarts in the window before the loop guard stops them.", Group: GroupChatApps, Order: 1880, Advanced: true}, Default: 3, Unit: "restarts"})

// Time span over which restarts are counted
var GatewayRestartLoopGuardWindowSeconds = Register(&Duration{Meta: Meta{Key: "gateway.restart_loop_guard.window_seconds", Label: "Restart loop window", Short: "Time span over which restarts are counted.", Group: GroupChatApps, Order: 1890, Advanced: true}, Default: time.Minute})

// Suspend a hosted relay-only machine once the service sits idle
var GatewayScaleToZeroEnabled = Register(&Bool{Meta: Meta{Key: "gateway.scale_to_zero.enabled", Label: "Idle suspend", Short: "Suspend this hosted machine once the service sits idle.", Help: "Only when every chat app goes through the relay and a wake address is registered.", Group: GroupChatApps, Order: 1895, Apply: Restart, Advanced: true, Env: "ICEBATH_SCALE_TO_ZERO"}})

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
var MattermostAllowedChannels = Register(&List{Meta: Meta{Key: "mattermost.allowed_channels", Env: "MATTERMOST_ALLOWED_CHANNELS", Label: "Allowed channels", Short: "Mattermost: only answer in these channel IDs.", Group: GroupChatApps, Order: 2100, Apply: Live, Advanced: true}})

// Mattermost: extra instructions for chosen channels
var MattermostChannelPrompts = Register(&Map{Meta: Meta{Key: "mattermost.channel_prompts", Label: "Channel prompts", Short: "Mattermost: extra instructions for chosen channels.", Help: "Maps channel ID to the instruction text.", Group: GroupChatApps, Order: 2110, Apply: Restart, Advanced: true}})

// Mattermost: channel IDs where it replies without a mention
var MattermostFreeResponseChannels = Register(&List{Meta: Meta{Key: "mattermost.free_response_channels", Env: "MATTERMOST_FREE_RESPONSE_CHANNELS", Label: "Free reply channels", Short: "Mattermost: channel IDs where it replies without a mention.", Group: GroupChatApps, Order: 2120, Apply: Live, Advanced: true}})

// Mattermost: only answer in channels when mentioned
var MattermostRequireMention = Register(&Bool{Meta: Meta{Key: "mattermost.require_mention", Env: "MATTERMOST_REQUIRE_MENTION", Label: "Require mention", Short: "Mattermost: only answer in channels when mentioned.", Group: GroupChatApps, Order: 2130, Apply: Live, Advanced: true}, Default: true})

// Mattermost: whether replies nest under the message they answer
var MattermostReplyMode = Register(&Enum{Meta: Meta{Key: "mattermost.reply_mode", Env: "MATTERMOST_REPLY_MODE", Label: "Reply mode", Short: "Mattermost: whether replies nest under the message they answer.", Group: GroupChatApps, Order: 2132, Apply: Restart, Advanced: true}, Default: "off", Values: []Choice{{Value: "off", Label: "Flat", Meaning: "Replies are plain channel messages."}, {Value: "thread", Label: "Threaded", Meaning: "Replies nest under your message."}}})

// Mattermost: user IDs allowed to talk to the bot
var MattermostAllowedUsers = Register(&List{Meta: Meta{Key: "mattermost.allowed_users", Env: "MATTERMOST_ALLOWED_USERS", Label: "Allowed users", Short: "Mattermost: user IDs allowed to talk to the bot.", Help: "The 26 character ID of a user, not the user name.", Group: GroupChatApps, Order: 2134, Advanced: true}})

// Mattermost: anyone may talk to the bot
var MattermostAllowAllUsers = Register(&Bool{Meta: Meta{Key: "mattermost.allow_all_users", Env: "MATTERMOST_ALLOW_ALL_USERS", Label: "Allow everyone", Short: "Mattermost: let anyone talk to the bot.", Group: GroupChatApps, Order: 2136, Advanced: true}, Default: false})

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

// Telegram: text sent this soon after a reply starts waits for it instead of interrupting
var TelegramFollowupGraceSeconds = Register(&Duration{Meta: Meta{Key: "telegram.followup_grace_seconds", Label: "Follow-up grace", Short: "Telegram: a follow-up this soon after a reply starts waits for it.", Help: "Zero lets every message interrupt at once.", Group: GroupChatApps, Order: 2335, Advanced: true, Env: "ICEBATH_TELEGRAM_FOLLOWUP_GRACE_SECONDS"}, Default: 3 * time.Second, Min: at(time.Duration(0))})

// Telegram: react to messages while working on them
var TelegramReactions = Register(&Bool{Meta: Meta{Key: "telegram.reactions", Label: "Reactions", Short: "Telegram: react to messages while working on them.", Group: GroupChatApps, Order: 2340, Apply: Live, Advanced: true}, Default: false})

// What the background service does once every chat app has disconnected
var GatewayOnAllAdaptersDown = Register(&Enum{Meta: Meta{Key: "gateway.on_all_adapters_down", Label: "All chats down", Short: "What the background service does once every chat app has dropped.", Group: GroupChatApps, Order: 2000, Advanced: true, Apply: Restart}, Default: "exit", Values: []Choice{{Value: "exit", Label: "Exit", Meaning: "Stop the background service."}, {Value: "stay_alive", Label: "Keep running", Meaning: "Keep running and keep reconnecting."}}})

// Address other machines use to reach this machine's shared rooms
var GatewayRoomLinkURL = Register(&String{Meta: Meta{Key: "gateway.room_link_url", Label: "Room link address", Short: "Address other machines use to reach the rooms shared from here.", Help: "Plain http is allowed only on this machine. A named profile without its own takes the main profile's.", Group: GroupChatApps, Order: 2010, Advanced: true}, Null: "rooms are not offered to other machines"})

// Telegram: answer in groups only when mentioned
var TelegramRequireMention = Register(&Bool{Meta: Meta{Key: "telegram.require_mention", Label: "Require mention", Short: "Telegram: answer in groups only when someone mentions the bot.", Group: GroupChatApps, Order: 2340, Advanced: true}, Default: false})

// Telegram: words that count as a mention besides the bot's name
var TelegramMentionPatterns = Register(&List{Meta: Meta{Key: "telegram.mention_patterns", Label: "Mention words", Short: "Telegram: words that count as a mention besides the bot's name.", Help: "Each item is a pattern matched against the message text.", Group: GroupChatApps, Order: 2340, Advanced: true}, Whole: true})

// Telegram API addresses tried when the usual one cannot be reached
var TelegramFallbackIPs = Register(&List{Meta: Meta{Key: "telegram.fallback_ips", Label: "Fallback addresses", Short: "Telegram: API addresses tried when the usual one is unreachable.", Help: "Empty finds them by itself.", Group: GroupChatApps, Order: 2342, Advanced: true, Apply: Restart, Env: "TELEGRAM_FALLBACK_IPS"}})

// Telegram never tries other API addresses
var TelegramDisableFallbackIPs = Register(&Bool{Meta: Meta{Key: "telegram.disable_fallback_ips", Label: "No fallback addresses", Short: "Telegram: never try other API addresses.", Group: GroupChatApps, Order: 2343, Advanced: true, Apply: Restart, Env: "ICEBATH_TELEGRAM_DISABLE_FALLBACK_IPS"}, Default: false})

// Longest the search for Telegram fallback addresses may take
var TelegramFallbackDiscoveryTimeout = Register(&Duration{Meta: Meta{Key: "telegram.fallback_discovery_timeout", Label: "Fallback search limit", Short: "Telegram: longest the search for fallback addresses may take.", Group: GroupChatApps, Order: 2344, Advanced: true, Apply: Restart, Env: "ICEBATH_TELEGRAM_FALLBACK_DISCOVERY_TIMEOUT"}, Default: 5 * time.Second, Min: at(time.Duration(0))})

// Longest one attempt to start the Telegram bot may take
var TelegramInitTimeout = Register(&Duration{Meta: Meta{Key: "telegram.init_timeout", Label: "Start limit", Short: "Telegram: longest one attempt to start the bot may take.", Group: GroupChatApps, Order: 2345, Advanced: true, Apply: Restart, Env: "ICEBATH_TELEGRAM_INIT_TIMEOUT"}, Default: 30 * time.Second})

// Most connections to the Telegram API at once
var TelegramHTTPPoolSize = Register(&Int{Meta: Meta{Key: "telegram.http_pool_size", Label: "Connection pool", Short: "Telegram: most connections to the Telegram API at once.", Group: GroupChatApps, Order: 2346, Advanced: true, Apply: Restart, Env: "ICEBATH_TELEGRAM_HTTP_POOL_SIZE"}, Default: 512, Min: at(1), Unit: "connections"})

// Longest a Telegram request waits for a free connection
var TelegramHTTPPoolTimeout = Register(&Duration{Meta: Meta{Key: "telegram.http_pool_timeout", Label: "Pool wait", Short: "Telegram: longest a request waits for a free connection.", Group: GroupChatApps, Order: 2347, Advanced: true, Apply: Restart, Env: "ICEBATH_TELEGRAM_HTTP_POOL_TIMEOUT"}, Default: 8 * time.Second})

// Longest connecting to the Telegram API may take
var TelegramHTTPConnectTimeout = Register(&Duration{Meta: Meta{Key: "telegram.http_connect_timeout", Label: "Connect limit", Short: "Telegram: longest connecting to the Telegram API may take.", Group: GroupChatApps, Order: 2348, Advanced: true, Apply: Restart, Env: "ICEBATH_TELEGRAM_HTTP_CONNECT_TIMEOUT"}, Default: 10 * time.Second})

// Longest the wait for an answer of the Telegram API may take
var TelegramHTTPReadTimeout = Register(&Duration{Meta: Meta{Key: "telegram.http_read_timeout", Label: "Read limit", Short: "Telegram: longest the wait for an API answer may take.", Group: GroupChatApps, Order: 2349, Advanced: true, Apply: Restart, Env: "ICEBATH_TELEGRAM_HTTP_READ_TIMEOUT"}, Default: 20 * time.Second})

// Longest sending a request to the Telegram API may take
var TelegramHTTPWriteTimeout = Register(&Duration{Meta: Meta{Key: "telegram.http_write_timeout", Label: "Write limit", Short: "Telegram: longest sending a request to the API may take.", Group: GroupChatApps, Order: 2350, Advanced: true, Apply: Restart, Env: "ICEBATH_TELEGRAM_HTTP_WRITE_TIMEOUT"}, Default: 20 * time.Second})

// Wait for more photos or files of one Telegram album
var TelegramMediaBatchDelay = Register(&Duration{Meta: Meta{Key: "telegram.media_batch_delay_seconds", Label: "Media gather wait", Short: "Telegram: wait for more photos or files of one album.", Group: GroupChatApps, Order: 2351, Advanced: true, Apply: Restart, Env: "ICEBATH_TELEGRAM_MEDIA_BATCH_DELAY_SECONDS"}, Default: 800 * time.Millisecond})

// Wait for more text of a Telegram message sent in parts
var TelegramTextBatchDelay = Register(&Duration{Meta: Meta{Key: "telegram.text_batch_delay_seconds", Label: "Text gather wait", Short: "Telegram: wait for more text of a message sent in parts.", Group: GroupChatApps, Order: 2352, Advanced: true, Apply: Restart, Env: "ICEBATH_TELEGRAM_TEXT_BATCH_DELAY_SECONDS"}, Default: 300 * time.Millisecond, Min: at(80 * time.Millisecond), Max: at(2 * time.Second)})

// Wait after a part of a Telegram message that hit the length limit
var TelegramTextBatchSplitDelay = Register(&Duration{Meta: Meta{Key: "telegram.text_batch_split_delay_seconds", Label: "Long text wait", Short: "Telegram: wait after a part that hit the length limit.", Help: "Never shorter than the text gather wait.", Group: GroupChatApps, Order: 2353, Advanced: true, Apply: Restart, Env: "ICEBATH_TELEGRAM_TEXT_BATCH_SPLIT_DELAY_SECONDS"}, Default: time.Second, Min: at(time.Duration(0)), Max: at(4 * time.Second)})

// Public address Telegram sends updates to
var TelegramWebhookURL = Register(&String{Meta: Meta{Key: "telegram.webhook_url", Label: "Webhook URL", Short: "Telegram: public address Telegram sends updates to.", Help: "Empty asks Telegram for updates instead. Needs the TELEGRAM_WEBHOOK_SECRET key.", Group: GroupChatApps, Order: 2354, Advanced: true, Apply: Restart, Env: "TELEGRAM_WEBHOOK_URL"}})

// Address the Telegram webhook listener binds to
var TelegramWebhookHost = Register(&String{Meta: Meta{Key: "telegram.webhook_host", Label: "Webhook address", Short: "Telegram: address the webhook listener binds to.", Help: "Empty listens on every address.", Group: GroupChatApps, Order: 2355, Advanced: true, Apply: Restart, Env: "TELEGRAM_WEBHOOK_HOST"}})

// Port the Telegram webhook listener binds to
var TelegramWebhookPort = Register(&Int{Meta: Meta{Key: "telegram.webhook_port", Label: "Webhook port", Short: "Telegram: port the webhook listener binds to.", Group: GroupChatApps, Order: 2356, Advanced: true, Apply: Restart, Env: "TELEGRAM_WEBHOOK_PORT"}, Default: 8443, Min: at(1), Max: at(65535)})

// Telegram: a mention of another bot means the message is not for this one
var TelegramExclusiveBotMentions = Register(&Bool{Meta: Meta{Key: "telegram.exclusive_bot_mentions", Label: "Other bot mentions", Short: "Telegram: skip messages that mention only another bot.", Group: GroupChatApps, Order: 2340, Advanced: true}, Default: true})

// Telegram: which messages from other bots it answers
var TelegramAllowBots = Register(&Enum{Meta: Meta{Key: "telegram.allow_bots", Env: "TELEGRAM_ALLOW_BOTS", Label: "Answer bots", Short: "Telegram: which messages from other bots it answers.", Group: GroupChatApps, Order: 2340, Advanced: true}, Default: "none", Values: []Choice{{Value: "none", Label: "None", Meaning: "Ignore other bots."}, {Value: "mentions", Label: "Mentions", Meaning: "Answer bots that mention it."}, {Value: "all", Label: "All", Meaning: "Answer every bot."}}})

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
var TelegramAllowFrom = Register(&List{Meta: Meta{Key: "telegram.allow_from", Env: "TELEGRAM_ALLOWED_USERS", Label: "Allowed people", Short: "Telegram: user IDs allowed to talk to it; * allows everyone.", Help: "Empty leaves access to pairing and the shared allow list.", Group: GroupChatApps, Order: 2340, Advanced: true}})

// Telegram: people allowed to talk to it in groups
var TelegramGroupAllowFrom = Register(&List{Meta: Meta{Key: "telegram.group_allow_from", Env: "TELEGRAM_GROUP_ALLOWED_USERS", Label: "Allowed in groups", Short: "Telegram: user IDs allowed to talk to it in groups.", Group: GroupChatApps, Order: 2340, Advanced: true}})

// Telegram: groups where everyone may talk to it
var TelegramGroupAllowedChats = Register(&List{Meta: Meta{Key: "telegram.group_allowed_chats", Env: "TELEGRAM_GROUP_ALLOWED_CHATS", Label: "Open groups", Short: "Telegram: group chat IDs where everyone may talk to it.", Group: GroupChatApps, Order: 2340, Advanced: true}})

// Telegram: whether replies make the phone notify
var TelegramNotifications = Register(&Enum{Meta: Meta{Key: "telegram.notifications", Label: "Reply alerts", Short: "Telegram: which replies make the phone buzz.", Group: GroupChatApps, Order: 2340, Advanced: true}, Default: "important", Values: []Choice{{Value: "important", Label: "Important", Meaning: "Only the final answer and questions."}, {Value: "all", Label: "All", Meaning: "Every message, progress included."}}})

// Discord: people allowed to talk to it
var DiscordAllowFrom = Register(&List{Meta: Meta{Key: "discord.allow_from", Label: "Allowed people", Short: "Discord: user IDs or names allowed to talk to it; * allows everyone.", Help: "Empty leaves access to pairing and the shared allow list.", Group: GroupChatApps, Order: 1220, Advanced: true, Env: "DISCORD_ALLOWED_USERS"}})

// Discord: server roles whose members may talk to it
var DiscordAllowedRoles = Register(&List{Meta: Meta{Key: "discord.allowed_roles", Label: "Allowed roles", Short: "Discord: server role IDs whose members may talk to it.", Group: GroupChatApps, Order: 1220, Advanced: true, Env: "DISCORD_ALLOWED_ROLES"}})

// Discord: let everyone talk to it
var DiscordAllowAllUsers = Register(&Bool{Meta: Meta{Key: "discord.allow_all_users", Label: "Allow everyone", Short: "Discord: let anyone who can see the bot talk to it.", Group: GroupChatApps, Order: 1220, Advanced: true, Env: "DISCORD_ALLOW_ALL_USERS"}, Default: false})

// Discord: which messages from other bots it answers
var DiscordAllowBots = Register(&Enum{Meta: Meta{Key: "discord.allow_bots", Label: "Answer bots", Short: "Discord: which messages from other bots it answers.", Group: GroupChatApps, Order: 1220, Advanced: true, Env: "DISCORD_ALLOW_BOTS"}, Default: "none", Values: []Choice{{Value: "none", Label: "None", Meaning: "Ignore other bots."}, {Value: "mentions", Label: "Mentions", Meaning: "Answer bots that mention it."}, {Value: "all", Label: "All", Meaning: "Answer every bot."}}})

// Discord: channels it never reads
var DiscordIgnoredChannels = Register(&List{Meta: Meta{Key: "discord.ignored_channels", Label: "Ignored channels", Short: "Discord: channel IDs or names it never reads.", Group: GroupChatApps, Order: 1220, Advanced: true}})

// Discord: channels where it answers in place instead of opening a thread
var DiscordNoThreadChannels = Register(&List{Meta: Meta{Key: "discord.no_thread_channels", Label: "No thread channels", Short: "Discord: channels where it answers in place, without a thread.", Group: GroupChatApps, Order: 1220, Advanced: true}})

// Matrix: address of the homeserver the bot account lives on
var MatrixHomeserver = Register(&String{Meta: Meta{Key: "matrix.homeserver", Env: "MATRIX_HOMESERVER", Label: "Homeserver", Short: "Matrix: address of the server the bot account lives on.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}})

// Matrix: user ID of the bot account
var MatrixUserID = Register(&String{Meta: Meta{Key: "matrix.user_id", Env: "MATRIX_USER_ID", Label: "Bot user ID", Short: "Matrix: user ID of the bot account, such as @bot:example.org.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}})

// Matrix: device ID the bot signs in as
var MatrixDeviceID = Register(&String{Meta: Meta{Key: "matrix.device_id", Env: "MATRIX_DEVICE_ID", Label: "Device ID", Short: "Matrix: device ID the bot signs in as, kept for encryption.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}})

// Matrix: people allowed to talk to it
var MatrixAllowedUsers = Register(&List{Meta: Meta{Key: "matrix.allowed_users", Env: "MATRIX_ALLOWED_USERS", Label: "Allowed people", Short: "Matrix: user IDs allowed to talk to it; * allows everyone.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}})

// Matrix: start a thread for each new conversation
var MatrixAutoThread = Register(&Bool{Meta: Meta{Key: "matrix.auto_thread", Label: "Auto threads", Short: "Matrix: answer each new conversation in its own thread.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}, Default: true})

// Matrix: open a thread when someone mentions it in a direct chat
var MatrixDmMentionThreads = Register(&Bool{Meta: Meta{Key: "matrix.dm_mention_threads", Label: "Threads in DMs", Short: "Matrix: open a thread when mentioned in a direct chat.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}, Default: false})

// Matrix: what one conversation covers
var MatrixSessionScope = Register(&Enum{Meta: Meta{Key: "matrix.session_scope", Label: "Conversation scope", Short: "Matrix: whether a conversation covers a whole room or one thread.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart, Env: "MATRIX_SESSION_SCOPE"}, Default: "auto", Values: []Choice{{Value: "auto", Label: "Auto", Meaning: "A thread when there is one, else the room."}, {Value: "room", Label: "Room", Meaning: "One conversation per room."}, {Value: "thread", Label: "Thread", Meaning: "One conversation per thread."}}})

// Matrix: read notice messages from other bots
var MatrixProcessNotices = Register(&Bool{Meta: Meta{Key: "matrix.process_notices", Label: "Read notices", Short: "Matrix: treat notice messages, which bots send, as messages to it.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}, Default: false})

// Matrix: senders it ignores, by pattern
var MatrixIgnoreUserPatterns = Register(&List{Meta: Meta{Key: "matrix.ignore_user_patterns", Label: "Ignored senders", Short: "Matrix: patterns of user IDs whose messages it ignores.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}})

// Matrix: longest message it sends before splitting
var MatrixMaxMessageLength = Register(&Int{Meta: Meta{Key: "matrix.max_message_length", Label: "Message length", Short: "Matrix: longest message it sends before splitting a reply.", Help: "Unset uses the length the server allows.", Group: GroupChatApps, Order: 2070, Advanced: true, Apply: Restart}, Unit: "characters", Null: "the server's limit"})

// WhatsApp: who may talk to it in direct chats
var WhatsappDmPolicy = Register(&Enum{Meta: Meta{Key: "whatsapp.dm_policy", Env: "WHATSAPP_DM_POLICY", Label: "Direct chat access", Short: "WhatsApp: who may talk to it in direct chats.", Group: GroupChatApps, Order: 2350, Advanced: true, Apply: Restart}, Default: "pairing", Values: whatsappPolicyChoices})

// WhatsApp: who may talk to it in groups
var WhatsappGroupPolicy = Register(&Enum{Meta: Meta{Key: "whatsapp.group_policy", Env: "WHATSAPP_GROUP_POLICY", Label: "Group access", Short: "WhatsApp: who may talk to it in groups.", Group: GroupChatApps, Order: 2350, Advanced: true, Apply: Restart}, Default: "pairing", Values: whatsappPolicyChoices})

// WhatsApp: people allowed to talk to it in direct chats
var WhatsappAllowFrom = Register(&List{Meta: Meta{Key: "whatsapp.allow_from", Env: "WHATSAPP_ALLOWED_USERS", Label: "Allowed people", Short: "WhatsApp: phone numbers allowed to talk to it; * allows everyone.", Group: GroupChatApps, Order: 2350, Advanced: true}})

// WhatsApp: people allowed to talk to it in groups
var WhatsappGroupAllowFrom = Register(&List{Meta: Meta{Key: "whatsapp.group_allow_from", Label: "Allowed in groups", Short: "WhatsApp: group IDs or numbers allowed to talk to it in groups.", Group: GroupChatApps, Order: 2350, Advanced: true}})

// WhatsApp: answer in groups only when mentioned
var WhatsappRequireMention = Register(&Bool{Meta: Meta{Key: "whatsapp.require_mention", Label: "Require mention", Short: "WhatsApp: answer in groups only when someone mentions it.", Group: GroupChatApps, Order: 2350, Advanced: true}, Default: false})

// WhatsApp: words that count as a mention
var WhatsappMentionPatterns = Register(&List{Meta: Meta{Key: "whatsapp.mention_patterns", Label: "Mention words", Short: "WhatsApp: words that count as a mention besides its name.", Group: GroupChatApps, Order: 2350, Advanced: true}, Whole: true})

// WhatsApp: chats where it answers without a mention
var WhatsappFreeResponseChats = Register(&List{Meta: Meta{Key: "whatsapp.free_response_chats", Label: "Free reply chats", Short: "WhatsApp: chats where it answers without being mentioned.", Group: GroupChatApps, Order: 2350, Advanced: true}})

// Buzz: address of the relay the bot talks through
var BuzzRelayURL = Register(&String{Meta: Meta{Key: "buzz.relay_url", Label: "Relay address", Short: "Buzz: address of the relay the bot talks through.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart, Env: "BUZZ_RELAY_URL"}})

// Buzz: the buzz program it runs
var BuzzCliPath = Register(&String{Meta: Meta{Key: "buzz.cli_path", Label: "Buzz program", Short: "Buzz: path of the buzz program; empty finds it on the PATH.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart, Env: "BUZZ_CLI_PATH"}})

// Buzz: how it receives messages
var BuzzTransport = Register(&Enum{Meta: Meta{Key: "buzz.transport", Label: "Connection type", Short: "Buzz: how it receives messages from the relay.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart, Env: "BUZZ_TRANSPORT"}, Default: "auto", Values: []Choice{{Value: "auto", Label: "Auto", Meaning: "A live connection, polling when it fails."}, {Value: "websocket", Label: "Live", Meaning: "Only a live connection."}, {Value: "poll", Label: "Poll", Meaning: "Ask the relay every few seconds."}}})

// Buzz: seconds between checks when polling
var BuzzPollInterval = Register(&Float{Meta: Meta{Key: "buzz.poll_interval", Label: "Poll interval", Short: "Buzz: seconds between checks for new messages when polling.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart, Env: "BUZZ_POLL_INTERVAL"}, Unit: "seconds", Null: "the built-in interval"})

// Buzz: channels it listens in
var BuzzChannels = Register(&List{Meta: Meta{Key: "buzz.channels", Label: "Channels", Short: "Buzz: channels it listens in.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart, Env: "BUZZ_CHANNELS"}})

// Buzz: people allowed to talk to it
var BuzzAllowedUsers = Register(&List{Meta: Meta{Key: "buzz.allowed_users", Label: "Allowed people", Short: "Buzz: public keys of people allowed to talk to it.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart, Env: "BUZZ_ALLOWED_USERS"}})

// Buzz: people whose messages it only reacts to
var BuzzReactionOnlyUsers = Register(&List{Meta: Meta{Key: "buzz.reaction_only_users", Label: "React only to", Short: "Buzz: public keys whose messages get a reaction but no reply.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart, Env: "BUZZ_REACTION_ONLY_USERS"}})

// Buzz: let everyone talk to it
var BuzzAllowAllUsers = Register(&Bool{Meta: Meta{Key: "buzz.allow_all_users", Label: "Allow everyone", Short: "Buzz: let anyone in its channels talk to it.", Group: GroupChatApps, Order: 1100, Advanced: true, Env: "BUZZ_ALLOW_ALL_USERS"}, Default: false})

// Buzz: answer in channels only when mentioned
var BuzzRequireMention = Register(&Bool{Meta: Meta{Key: "buzz.require_mention", Label: "Require mention", Short: "Buzz: answer in channels only when someone mentions it.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart, Env: "BUZZ_REQUIRE_MENTION"}, Default: true})

// Buzz: answer in a thread under the message
var BuzzReplyInThread = Register(&Bool{Meta: Meta{Key: "buzz.reply_in_thread", Label: "Reply in thread", Short: "Buzz: answer in a thread under the message; off answers in place.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart, Env: "BUZZ_REPLY_IN_THREAD"}, Default: true})

// Buzz: which reply parts point back at the message
var BuzzReplyToMode = Register(&Enum{Meta: Meta{Key: "buzz.reply_to_mode", Label: "Reply quoting", Short: "Buzz: which parts of a reply point back at your message.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart, Env: "BUZZ_REPLY_TO_MODE"}, Default: "first", Values: []Choice{{Value: "first", Label: "First", Meaning: "Only the first part."}, {Value: "all", Label: "All", Meaning: "Every part."}, {Value: "off", Label: "Off", Meaning: "None."}}})

// Buzz: extra hosts attachments may be downloaded from
var BuzzAttachmentHosts = Register(&List{Meta: Meta{Key: "buzz.attachment_hosts", Label: "Attachment hosts", Short: "Buzz: extra hosts attachments may be downloaded from.", Help: "The relay's own host is always allowed.", Group: GroupChatApps, Order: 1100, Advanced: true, Apply: Restart}})

// WeCom: ID of the smart robot
var WecomBotID = Register(&String{Meta: Meta{Key: "wecom.bot_id", Env: "WECOM_BOT_ID", Label: "Robot ID", Short: "WeCom: ID of the smart robot the bot runs as.", Group: GroupChatApps, Order: 2400, Advanced: true, Apply: Restart}})

// WeCom: live connection address
var WecomWebsocketURL = Register(&String{Meta: Meta{Key: "wecom.websocket_url", Env: "WECOM_WEBSOCKET_URL", Label: "Connection address", Short: "WeCom: address of the live connection; empty uses WeCom's own.", Group: GroupChatApps, Order: 2400, Advanced: true, Apply: Restart}})

// WeCom: who may message the bot directly
var WecomDmPolicy = Register(&Enum{Meta: Meta{Key: "wecom.dm_policy", Env: "WECOM_DM_POLICY", Label: "Direct messages", Short: "WeCom: who may message the bot directly.", Group: GroupChatApps, Order: 2401, Advanced: true, Apply: Restart}, Default: "pairing", Open: true, Values: chatPolicyChoices})

// WeCom: people allowed to message the bot directly
var WecomAllowFrom = Register(&List{Meta: Meta{Key: "wecom.allow_from", Env: "WECOM_ALLOWED_USERS", Label: "Allowed people", Short: "WeCom: user IDs allowed to message the bot directly.", Group: GroupChatApps, Order: 2402, Advanced: true, Apply: Restart}})

// WeCom open to every sender
var WecomAllowAllUsers = Register(&Bool{Meta: Meta{Key: "wecom.allow_all_users", Env: "WECOM_ALLOW_ALL_USERS", Label: "Allow everyone", Short: "WeCom: let anyone message the bot.", Group: GroupChatApps, Order: 2403, Advanced: true}, Default: false})

// WeCom: which group chats the bot answers in
var WecomGroupPolicy = Register(&Enum{Meta: Meta{Key: "wecom.group_policy", Env: "WECOM_GROUP_POLICY", Label: "Group chats", Short: "WeCom: which group chats the bot answers in.", Group: GroupChatApps, Order: 2404, Advanced: true, Apply: Restart}, Default: "pairing", Open: true, Values: chatPolicyChoices})

// WeCom: group chats the bot answers in
var WecomGroupAllowFrom = Register(&List{Meta: Meta{Key: "wecom.group_allow_from", Label: "Allowed groups", Short: "WeCom: group chat IDs the bot answers in.", Group: GroupChatApps, Order: 2405, Advanced: true, Apply: Restart}})

// Wait for more text of a WeCom message sent in parts
var WecomTextBatchDelay = Register(&Duration{Meta: Meta{Key: "wecom.text_batch_delay_seconds", Env: "ICEBATH_WECOM_TEXT_BATCH_DELAY_SECONDS", Label: "Text gather wait", Short: "WeCom: wait for more text of a message sent in parts.", Group: GroupChatApps, Order: 2406, Advanced: true, Apply: Restart}, Default: 600 * time.Millisecond, Min: at(time.Duration(0))})

// Wait after a part of a WeCom message that hit the length limit
var WecomTextBatchSplitDelay = Register(&Duration{Meta: Meta{Key: "wecom.text_batch_split_delay_seconds", Env: "ICEBATH_WECOM_TEXT_BATCH_SPLIT_DELAY_SECONDS", Label: "Long text wait", Short: "WeCom: wait after a part that hit the length limit.", Group: GroupChatApps, Order: 2407, Advanced: true, Apply: Restart}, Default: 2 * time.Second, Min: at(time.Duration(0))})

// WeCom callback: people allowed to message the app
var WecomCallbackAllowedUsers = Register(&List{Meta: Meta{Key: "wecom_callback.allowed_users", Env: "WECOM_CALLBACK_ALLOWED_USERS", Label: "Allowed people", Short: "WeCom callback: user IDs allowed to message the app.", Group: GroupChatApps, Order: 2411, Advanced: true}})

// WeCom callback open to every sender
var WecomCallbackAllowAllUsers = Register(&Bool{Meta: Meta{Key: "wecom_callback.allow_all_users", Env: "WECOM_CALLBACK_ALLOW_ALL_USERS", Label: "Allow everyone", Short: "WeCom callback: let anyone message the app.", Group: GroupChatApps, Order: 2412, Advanced: true}, Default: false})

// WeCom app: company ID
var WecomCallbackCorpID = Register(&String{Meta: Meta{Key: "wecom_callback.corp_id", Env: "WECOM_CALLBACK_CORP_ID", Label: "Company ID", Short: "WeCom app: ID of your company in WeCom.", Group: GroupChatApps, Order: 2410, Advanced: true, Apply: Restart}})

// WeCom app: agent ID of the app
var WecomCallbackAgentID = Register(&String{Meta: Meta{Key: "wecom_callback.agent_id", Env: "WECOM_CALLBACK_AGENT_ID", Label: "App ID", Short: "WeCom app: agent ID of the self-built app.", Group: GroupChatApps, Order: 2410, Advanced: true, Apply: Restart}})

// WeCom app: address the callback server listens on
var WecomCallbackHost = Register(&String{Meta: Meta{Key: "wecom_callback.host", Env: "WECOM_CALLBACK_HOST", Label: "Listen address", Short: "WeCom app: address the message receiver listens on.", Group: GroupChatApps, Order: 2410, Advanced: true, Apply: Restart}})

// WeCom app: port the callback server listens on
var WecomCallbackPort = Register(&Int{Meta: Meta{Key: "wecom_callback.port", Env: "WECOM_CALLBACK_PORT", Label: "Listen port", Short: "WeCom app: port the message receiver listens on.", Group: GroupChatApps, Order: 2410, Advanced: true, Apply: Restart}, Default: 8645, Min: at(1), Max: at(65535)})

// WhatsApp Business: phone number ID
var WhatsappCloudPhoneNumberID = Register(&String{Meta: Meta{Key: "whatsapp_cloud.phone_number_id", Env: "WHATSAPP_CLOUD_PHONE_NUMBER_ID", Label: "Phone number ID", Short: "WhatsApp Business: phone number ID from the Meta app dashboard.", Group: GroupChatApps, Order: 2360, Advanced: true, Apply: Restart}})

// WhatsApp Business: app ID
var WhatsappCloudAppID = Register(&String{Meta: Meta{Key: "whatsapp_cloud.app_id", Env: "WHATSAPP_CLOUD_APP_ID", Label: "App ID", Short: "WhatsApp Business: ID of your Meta app.", Group: GroupChatApps, Order: 2360, Advanced: true, Apply: Restart}})

// WhatsApp Business: business account ID
var WhatsappCloudWabaID = Register(&String{Meta: Meta{Key: "whatsapp_cloud.waba_id", Env: "WHATSAPP_CLOUD_WABA_ID", Label: "Business account", Short: "WhatsApp Business: ID of the business account.", Group: GroupChatApps, Order: 2360, Advanced: true, Apply: Restart}})

// WhatsApp Business: address the webhook receiver listens on
var WhatsappCloudWebhookHost = Register(&String{Meta: Meta{Key: "whatsapp_cloud.webhook_host", Env: "WHATSAPP_CLOUD_WEBHOOK_HOST", Label: "Listen address", Short: "WhatsApp Business: address the message receiver listens on.", Group: GroupChatApps, Order: 2360, Advanced: true, Apply: Restart}})

// WhatsApp Business: port the webhook receiver listens on
var WhatsappCloudWebhookPort = Register(&Int{Meta: Meta{Key: "whatsapp_cloud.webhook_port", Env: "WHATSAPP_CLOUD_WEBHOOK_PORT", Label: "Listen port", Short: "WhatsApp Business: port the message receiver listens on.", Group: GroupChatApps, Order: 2360, Advanced: true, Apply: Restart}, Min: at(1), Max: at(65535), Null: "the built-in port"})

// WhatsApp Business: path the webhook receiver answers on
var WhatsappCloudWebhookPath = Register(&String{Meta: Meta{Key: "whatsapp_cloud.webhook_path", Env: "WHATSAPP_CLOUD_WEBHOOK_PATH", Label: "Webhook path", Short: "WhatsApp Business: path the message receiver answers on.", Group: GroupChatApps, Order: 2360, Advanced: true, Apply: Restart}})

// WhatsApp Business: Graph API version
var WhatsappCloudAPIVersion = Register(&String{Meta: Meta{Key: "whatsapp_cloud.api_version", Env: "WHATSAPP_CLOUD_API_VERSION", Label: "API version", Short: "WhatsApp Business: version of Meta's Graph API to call.", Group: GroupChatApps, Order: 2360, Advanced: true, Apply: Restart}})

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
var MattermostURL = Register(&String{Meta: Meta{Key: "mattermost.url", Env: "MATTERMOST_URL", Label: "Server address", Short: "Mattermost: address of your Mattermost server.", Help: "For example https://chat.example.com. The bot token stays with your other secrets.", Group: GroupChatApps, Order: 3100, Advanced: true, Apply: Restart}, Null: "not connected"})

// DingTalk: only answer group chats when triggered
var DingtalkRequireMention = Register(&Bool{Meta: Meta{Key: "dingtalk.require_mention", Label: "Require mention", Short: "DingTalk: only answer in group chats when mentioned.", Group: GroupChatApps, Order: 3200, Advanced: true}, Default: false})

// DingTalk: words that count as calling the bot
var DingtalkMentionPatterns = Register(&List{Meta: Meta{Key: "dingtalk.mention_patterns", Label: "Wake patterns", Short: "DingTalk: patterns in a message that count as mentioning it.", Help: "Regular expressions, matched without regard to case.", Group: GroupChatApps, Order: 3210, Advanced: true, Apply: Restart}, Whole: true})

// DingTalk: group chats answered without a mention
var DingtalkFreeResponseChats = Register(&List{Meta: Meta{Key: "dingtalk.free_response_chats", Label: "Free reply chats", Short: "DingTalk: group chat IDs where it replies without a mention.", Group: GroupChatApps, Order: 3220, Advanced: true}})

// DingTalk: the only group chats it answers in
var DingtalkAllowedChats = Register(&List{Meta: Meta{Key: "dingtalk.allowed_chats", Label: "Allowed chats", Short: "DingTalk: only answer in these group chat IDs.", Help: "Empty allows every group chat.", Group: GroupChatApps, Order: 3230, Advanced: true}})

// Feishu: which of the two clouds the app lives in
var FeishuDomain = Register(&Enum{Meta: Meta{Key: "feishu.domain", Label: "Cloud", Short: "Feishu: whether the app is on Feishu in China or on Lark.", Group: GroupChatApps, Order: 3300, Advanced: true, Apply: Restart}, Default: "feishu", Values: []Choice{{Value: "feishu", Label: "Feishu", Meaning: "open.feishu.cn"}, {Value: "lark", Label: "Lark", Meaning: "open.larksuite.com"}}})

// Feishu: how messages reach icebath
var FeishuConnectionMode = Register(&Enum{Meta: Meta{Key: "feishu.connection_mode", Label: "Connection", Short: "Feishu: receive messages over a socket or through a web address.", Group: GroupChatApps, Order: 3310, Advanced: true, Apply: Restart}, Default: "websocket", Values: []Choice{{Value: "websocket", Label: "Socket", Meaning: "No public address needed."}, {Value: "webhook", Label: "Web address", Meaning: "Feishu calls an address you expose."}}})

// BlueBubbles: address of the BlueBubbles server
var BluebubblesServerURL = Register(&String{Meta: Meta{Key: "bluebubbles.server_url", Label: "Server address", Short: "BlueBubbles: address of the BlueBubbles server on your Mac.", Help: "Such as http://192.168.1.10:1234.", Group: GroupChatApps, Order: 3395, Advanced: true, Apply: Restart, Env: "BLUEBUBBLES_SERVER_URL"}, Default: ""})

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

// Pause between the parts of a long Weixin reply
var WeixinSendChunkDelay = Register(&Duration{Meta: Meta{Key: "weixin.send_chunk_delay_seconds", Label: "Part gap", Short: "Weixin: pause between the parts of a long reply.", Group: GroupChatApps, Order: 3521, Advanced: true, Apply: Restart, Env: "WEIXIN_SEND_CHUNK_DELAY_SECONDS"}, Default: 1500 * time.Millisecond, Min: at(time.Duration(0))})

// Times a part of a Weixin reply is sent again after it fails
var WeixinSendChunkRetries = Register(&Int{Meta: Meta{Key: "weixin.send_chunk_retries", Label: "Part retries", Short: "Weixin: times a part of a reply is sent again after it fails.", Group: GroupChatApps, Order: 3522, Advanced: true, Apply: Restart, Env: "WEIXIN_SEND_CHUNK_RETRIES"}, Default: 4, Min: at(0)})

// Pause before a failed part of a Weixin reply is sent again
var WeixinSendChunkRetryDelay = Register(&Duration{Meta: Meta{Key: "weixin.send_chunk_retry_delay_seconds", Label: "Retry gap", Short: "Weixin: pause before a failed part is sent again.", Help: "Each further try waits longer.", Group: GroupChatApps, Order: 3523, Advanced: true, Apply: Restart, Env: "WEIXIN_SEND_CHUNK_RETRY_DELAY_SECONDS"}, Default: time.Second, Min: at(time.Duration(0))})

// Rate limit answers of Weixin that pause sending
var WeixinRateLimitCircuitThreshold = Register(&Int{Meta: Meta{Key: "weixin.rate_limit_circuit_threshold", Label: "Rate limit count", Short: "Weixin: rate limit answers that pause sending.", Group: GroupChatApps, Order: 3524, Advanced: true, Apply: Restart, Env: "WEIXIN_RATE_LIMIT_CIRCUIT_THRESHOLD"}, Default: 1, Min: at(1)})

// Time span in which Weixin rate limit answers are counted
var WeixinRateLimitCircuitWindow = Register(&Duration{Meta: Meta{Key: "weixin.rate_limit_circuit_window_seconds", Label: "Rate limit window", Short: "Weixin: time span in which rate limit answers are counted.", Group: GroupChatApps, Order: 3525, Advanced: true, Apply: Restart, Env: "WEIXIN_RATE_LIMIT_CIRCUIT_WINDOW_SECONDS"}, Default: 30 * time.Second, Min: at(time.Duration(0))})

// How long Weixin sending pauses after too many rate limit answers
var WeixinRateLimitCircuitOpen = Register(&Duration{Meta: Meta{Key: "weixin.rate_limit_circuit_open_seconds", Label: "Rate limit pause", Short: "Weixin: how long sending pauses after the rate limit count.", Help: "Zero never pauses.", Group: GroupChatApps, Order: 3526, Advanced: true, Apply: Restart, Env: "WEIXIN_RATE_LIMIT_CIRCUIT_OPEN_SECONDS"}, Default: 30 * time.Second, Min: at(time.Duration(0))})

// Signal: story posts skipped
var SignalIgnoreStories = Register(&Bool{Meta: Meta{Key: "signal.ignore_stories", Label: "Skip stories", Short: "Signal: ignore story posts from contacts.", Group: GroupChatApps, Order: 3800, Advanced: true, Apply: Restart, Env: "SIGNAL_IGNORE_STORIES"}, Default: true})

// Signal: group chats answered only when mentioned
var SignalRequireMention = Register(&Bool{Meta: Meta{Key: "signal.require_mention", Label: "Require mention", Short: "Signal: only answer in group chats when mentioned.", Group: GroupChatApps, Order: 3810, Advanced: true, Apply: Restart, Env: "SIGNAL_REQUIRE_MENTION"}, Default: false})

// Signal reactions that mark a message as being worked on and done
var SignalReactions = Register(&Bool{Meta: Meta{Key: "signal.reactions", Label: "Progress reactions", Short: "Signal: react to a message while working on it and when done.", Help: "People outside the allowed users never get a reaction.", Group: GroupChatApps, Order: 3820, Advanced: true, Env: "SIGNAL_REACTIONS"}, Default: true})

// Signal senders allowed to message the bot
var SignalAllowedUsers = Register(&List{Meta: Meta{Key: "signal.allowed_users", Label: "Allowed users", Short: "Signal: phone numbers or UUIDs allowed to message the bot.", Group: GroupChatApps, Order: 3830, Advanced: true, Apply: Restart, Env: "SIGNAL_ALLOWED_USERS"}})

// Signal open to every sender
var SignalAllowAllUsers = Register(&Bool{Meta: Meta{Key: "signal.allow_all_users", Label: "Allow everyone", Short: "Signal: let any Signal user message the bot.", Group: GroupChatApps, Order: 3840, Advanced: true, Env: "SIGNAL_ALLOW_ALL_USERS"}, Default: false})

// Signal groups the bot answers in
var SignalGroupAllowedUsers = Register(&List{Meta: Meta{Key: "signal.group_allowed_users", Label: "Allowed groups", Short: "Signal: group IDs the bot answers in.", Help: "Empty answers in no group; * answers in every group.", Group: GroupChatApps, Order: 3850, Advanced: true, Apply: Restart, Env: "SIGNAL_GROUP_ALLOWED_USERS"}})

// Yuanbao: socket address the bot connects to
var YuanbaoWsURL = Register(&String{Meta: Meta{Key: "yuanbao.ws_url", Label: "Socket address", Short: "Yuanbao: address of the socket the bot receives messages on.", Group: GroupChatApps, Order: 3900, Advanced: true, Apply: Restart}, Default: "wss://bot-wss.yuanbao.tencent.com/wss/connection"})

// Yuanbao: address of the bot service
var YuanbaoAPIDomain = Register(&String{Meta: Meta{Key: "yuanbao.api_domain", Label: "Service address", Short: "Yuanbao: address of the bot service that signs the bot in.", Group: GroupChatApps, Order: 3910, Advanced: true, Apply: Restart}, Default: "https://bot.yuanbao.tencent.com"})

// Yuanbao: service environment requests are routed to
var YuanbaoRouteEnv = Register(&String{Meta: Meta{Key: "yuanbao.route_env", Label: "Service route", Short: "Yuanbao: which service environment the bot's requests go to.", Group: GroupChatApps, Order: 3920, Advanced: true, Apply: Restart}, Null: "the production service"})

// Slack: messages from other bots
var SlackAllowBots = Register(&Enum{Meta: Meta{Key: "slack.allow_bots", Env: "SLACK_ALLOW_BOTS", Label: "Other bots", Short: "Slack: whether messages from other bots are answered.", Group: GroupChatApps, Order: 3070, Advanced: true}, Default: "none", Values: []Choice{{Value: "none", Label: "Never", Meaning: "Ignore other bots."}, {Value: "mentions", Label: "When mentioned", Meaning: "Answer bots that mention it."}, {Value: "all", Label: "Always", Meaning: "Answer every bot message."}}})

// Slack text patterns that count as mentioning the bot
var SlackMentionPatterns = Register(&List{Meta: Meta{Key: "slack.mention_patterns", Label: "Mention words", Short: "Slack: words that count as a mention besides the bot's name.", Help: "Each item is a pattern matched against the message text.", Group: GroupChatApps, Order: 3075, Advanced: true, Apply: Restart, Env: "SLACK_MENTION_PATTERNS"}, Whole: true, EnvSplit: true})

// How long Slack remembers a message it already handled
var SlackDedupTTL = Register(&Duration{Meta: Meta{Key: "slack.dedup_ttl_seconds", Label: "Repeat memory", Short: "Slack: how long a handled message is remembered, to skip repeats.", Help: "Slack sends a message again after a reconnect.", Group: GroupChatApps, Order: 3080, Advanced: true, Env: "SLACK_DEDUP_TTL_SECONDS"}, Default: time.Hour, Min: at(time.Millisecond), DefaultBelow: true})

// Slack users whose posts through the Web API count as a person writing
var SlackAPIHumanUsers = Register(&List{Meta: Meta{Key: "slack.api_human_users", Label: "API posters", Short: "Slack: user IDs whose API posts count as a person writing.", Group: GroupChatApps, Order: 3085, Advanced: true, Apply: Restart, Env: "SLACK_API_HUMAN_USERS"}})

// Slack users allowed to talk to the bot
var SlackAllowedUsers = Register(&List{Meta: Meta{Key: "slack.allowed_users", Label: "Allowed users", Short: "Slack: user IDs allowed to talk to the bot.", Group: GroupChatApps, Order: 3090, Advanced: true, Env: "SLACK_ALLOWED_USERS"}})

// Slack open to every user
var SlackAllowAllUsers = Register(&Bool{Meta: Meta{Key: "slack.allow_all_users", Label: "Allow everyone", Short: "Slack: let any user talk to the bot.", Group: GroupChatApps, Order: 3095, Advanced: true, Env: "SLACK_ALLOW_ALL_USERS"}, Default: false})

// DingTalk: users allowed to use the bot
var DingtalkAllowedUsers = Register(&List{Meta: Meta{Key: "dingtalk.allowed_users", Label: "Allowed users", Short: "DingTalk: user or staff IDs allowed to use the bot.", Help: "Empty or a star allows everyone.", Group: GroupChatApps, Order: 3240, Advanced: true, Apply: Restart, Env: "DINGTALK_ALLOWED_USERS"}})

// DingTalk: robot code replies and reactions are sent as
var DingtalkRobotCode = Register(&String{Meta: Meta{Key: "dingtalk.robot_code", Label: "Robot code", Short: "DingTalk: robot code replies and reactions are sent as.", Help: "Blank uses the app's client ID.", Group: GroupChatApps, Order: 3250, Advanced: true, Apply: Restart}, Default: ""})

// DingTalk: AI card template that streams and updates replies
var DingtalkCardTemplateID = Register(&String{Meta: Meta{Key: "dingtalk.card_template_id", Label: "AI card template", Short: "DingTalk: AI card template that streams and updates replies.", Help: "Blank sends plain messages, which cannot be edited.", Group: GroupChatApps, Order: 3260, Advanced: true, Apply: Restart}, Default: ""})

// DingTalk: address of the DingTalk open API
var DingtalkOpenapiEndpoint = Register(&String{Meta: Meta{Key: "dingtalk.openapi_endpoint", Label: "Open API address", Short: "DingTalk: address of the DingTalk open API.", Group: GroupChatApps, Order: 3270, Advanced: true, Apply: Restart, Env: "DINGTALK_OPENAPI_ENDPOINT"}, Default: "https://api.dingtalk.com"})

// DingTalk: address of the app registration service setup signs in through
var DingtalkRegistrationBaseURL = Register(&String{Meta: Meta{Key: "dingtalk.registration_base_url", Label: "Registration address", Short: "DingTalk: address of the app registration service setup uses.", Group: GroupChatApps, Order: 3280, Advanced: true, Env: "DINGTALK_REGISTRATION_BASE_URL"}, Default: "https://oapi.dingtalk.com"})

// DingTalk: source name setup gives the app registration service
var DingtalkRegistrationSource = Register(&String{Meta: Meta{Key: "dingtalk.registration_source", Label: "Registration source", Short: "DingTalk: source name setup gives the app registration service.", Group: GroupChatApps, Order: 3290, Advanced: true, Env: "DINGTALK_REGISTRATION_SOURCE"}, Default: "openClaw"})

// Feishu: messages from other bots
var FeishuAllowBots = Register(&Enum{Meta: Meta{Key: "feishu.allow_bots", Label: "Other bots", Short: "Feishu: whether messages from other bots are answered.", Group: GroupChatApps, Order: 3320, Advanced: true, Apply: Restart, Env: "FEISHU_ALLOW_BOTS"}, Default: "none", Values: []Choice{{Value: "none", Label: "Never", Meaning: "Ignore other bots."}, {Value: "mentions", Label: "When mentioned", Meaning: "Answer bots that mention it."}, {Value: "all", Label: "Always", Meaning: "Answer every bot message."}}})

// Feishu: user IDs allowed to use the bot
var FeishuAllowedUsers = Register(&List{Meta: Meta{Key: "feishu.allowed_users", Label: "Allowed users", Short: "Feishu: user IDs allowed to use the bot.", Help: "Empty lets anyone write to it directly; group chats follow Group access.", Group: GroupChatApps, Order: 3322, Advanced: true, Apply: Restart, Env: "FEISHU_ALLOWED_USERS"}})

// Feishu: anyone may write to the bot directly
var FeishuAllowAllUsers = Register(&Bool{Meta: Meta{Key: "feishu.allow_all_users", Label: "Allow everyone", Short: "Feishu: let anyone write to the bot directly.", Group: GroupChatApps, Order: 3324, Advanced: true, Apply: Restart, Env: "FEISHU_ALLOW_ALL_USERS"}, Default: false})

// Feishu: who may talk to the bot in group chats
var FeishuGroupPolicy = Register(&Enum{Meta: Meta{Key: "feishu.group_policy", Label: "Group access", Short: "Feishu: who may talk to the bot in group chats.", Help: "A rule under Group rules wins for its chat.", Group: GroupChatApps, Order: 3326, Advanced: true, Apply: Restart, Env: "FEISHU_GROUP_POLICY"}, Default: "allowlist", Values: []Choice{{Value: "allowlist", Label: "Allowed users", Meaning: "Only the allowed users."}, {Value: "open", Label: "Everyone", Meaning: "Anyone in the group."}, {Value: "blacklist", Label: "All but blocked", Meaning: "Anyone a group rule does not block."}, {Value: "admin_only", Label: "Admins only", Meaning: "Only the admins."}, {Value: "disabled", Label: "Nobody", Meaning: "No group messages, admins excepted."}}})

// Feishu: access rules of single group chats, keyed by chat ID
var FeishuGroupRules = Register(&Map{Meta: Meta{Key: "feishu.group_rules", Label: "Group rules", Short: "Feishu: access rules of single group chats, keyed by chat ID.", Help: "Each entry takes policy, allowlist, blacklist and require_mention.", Group: GroupChatApps, Order: 3328, Advanced: true, Apply: Restart}})

// Feishu: user IDs that pass every group rule
var FeishuAdmins = Register(&List{Meta: Meta{Key: "feishu.admins", Label: "Admins", Short: "Feishu: user IDs that pass every group rule.", Group: GroupChatApps, Order: 3330, Advanced: true, Apply: Restart}})

// Feishu: only answer in group chats when mentioned
var FeishuRequireMention = Register(&Bool{Meta: Meta{Key: "feishu.require_mention", Label: "Require mention", Short: "Feishu: only answer in group chats when mentioned.", Group: GroupChatApps, Order: 3332, Advanced: true, Apply: Restart, Env: "FEISHU_REQUIRE_MENTION"}, Default: true})

// Feishu: react to messages while working on them
var FeishuReactions = Register(&Bool{Meta: Meta{Key: "feishu.reactions", Label: "Reactions", Short: "Feishu: react to messages while working on them.", Group: GroupChatApps, Order: 3334, Advanced: true, Env: "FEISHU_REACTIONS"}, Default: true})

// Feishu: open ID of the bot, for telling its own messages and mentions apart
var FeishuBotOpenID = Register(&String{Meta: Meta{Key: "feishu.bot_open_id", Label: "Bot open ID", Short: "Feishu: open ID of the bot, to spot its own messages and mentions.", Help: "Found by itself when the app may read its own info.", Group: GroupChatApps, Order: 3336, Advanced: true, Apply: Restart, Env: "FEISHU_BOT_OPEN_ID"}})

// Feishu: user ID of the bot
var FeishuBotUserID = Register(&String{Meta: Meta{Key: "feishu.bot_user_id", Label: "Bot user ID", Short: "Feishu: user ID of the bot, to spot its own messages and mentions.", Group: GroupChatApps, Order: 3338, Advanced: true, Apply: Restart, Env: "FEISHU_BOT_USER_ID"}})

// Feishu: display name of the bot
var FeishuBotName = Register(&String{Meta: Meta{Key: "feishu.bot_name", Label: "Bot name", Short: "Feishu: display name of the bot, to spot mentions of it.", Help: "Found by itself when the app may read its own info.", Group: GroupChatApps, Order: 3340, Advanced: true, Apply: Restart, Env: "FEISHU_BOT_NAME"}})

// Feishu: address the event listener binds to
var FeishuWebhookHost = Register(&String{Meta: Meta{Key: "feishu.webhook_host", Label: "Listener address", Short: "Feishu: address the listener for events binds to.", Help: "Used when Connection is Webhook.", Group: GroupChatApps, Order: 3342, Advanced: true, Apply: Restart, Env: "FEISHU_WEBHOOK_HOST"}, Default: "127.0.0.1"})

// Feishu: port the event listener binds to
var FeishuWebhookPort = Register(&Int{Meta: Meta{Key: "feishu.webhook_port", Label: "Listener port", Short: "Feishu: port the listener for events binds to.", Group: GroupChatApps, Order: 3344, Advanced: true, Apply: Restart, Env: "FEISHU_WEBHOOK_PORT"}, Default: 8765, Min: at(1), Max: at(65535)})

// Feishu: path events are sent to
var FeishuWebhookPath = Register(&String{Meta: Meta{Key: "feishu.webhook_path", Label: "Listener path", Short: "Feishu: path Feishu sends events to.", Group: GroupChatApps, Order: 3346, Advanced: true, Apply: Restart, Env: "FEISHU_WEBHOOK_PATH"}, Default: "/feishu/webhook"})

// Feishu: how many handled message IDs are remembered to skip repeats
var FeishuDedupCacheSize = Register(&Int{Meta: Meta{Key: "feishu.dedup_cache_size", Label: "Repeat memory", Short: "Feishu: handled messages remembered so a repeat is skipped.", Group: GroupChatApps, Order: 3348, Advanced: true, Apply: Restart, Env: "ICEBATH_FEISHU_DEDUP_CACHE_SIZE"}, Default: 2048, Min: at(32), Unit: "messages"})

// Feishu: wait for more text before answering a message sent in parts
var FeishuTextBatchDelay = Register(&Duration{Meta: Meta{Key: "feishu.text_batch_delay_seconds", Label: "Text gather wait", Short: "Feishu: wait for more text before answering a message sent in parts.", Group: GroupChatApps, Order: 3350, Advanced: true, Apply: Restart, Env: "ICEBATH_FEISHU_TEXT_BATCH_DELAY_SECONDS"}, Default: 600 * time.Millisecond, Min: at(time.Duration(0))})

// Feishu: longer wait when a long message arrives split
var FeishuTextBatchSplitDelay = Register(&Duration{Meta: Meta{Key: "feishu.text_batch_split_delay_seconds", Label: "Split text wait", Short: "Feishu: longer wait when a long message arrives split by Feishu.", Group: GroupChatApps, Order: 3352, Advanced: true, Apply: Restart, Env: "ICEBATH_FEISHU_TEXT_BATCH_SPLIT_DELAY_SECONDS"}, Default: 2 * time.Second, Min: at(time.Duration(0))})

// Feishu: most messages gathered into one turn
var FeishuTextBatchMaxMessages = Register(&Int{Meta: Meta{Key: "feishu.text_batch_max_messages", Label: "Gathered messages", Short: "Feishu: most messages gathered into one turn.", Group: GroupChatApps, Order: 3354, Advanced: true, Apply: Restart, Env: "ICEBATH_FEISHU_TEXT_BATCH_MAX_MESSAGES"}, Default: 8, Min: at(1), Unit: "messages"})

// Feishu: most characters gathered into one turn
var FeishuTextBatchMaxChars = Register(&Int{Meta: Meta{Key: "feishu.text_batch_max_chars", Label: "Gathered characters", Short: "Feishu: most characters gathered into one turn.", Group: GroupChatApps, Order: 3356, Advanced: true, Apply: Restart, Env: "ICEBATH_FEISHU_TEXT_BATCH_MAX_CHARS"}, Default: 4000, Min: at(1), Unit: "characters"})

// Feishu: wait for more pictures or files before answering
var FeishuMediaBatchDelay = Register(&Duration{Meta: Meta{Key: "feishu.media_batch_delay_seconds", Label: "Media gather wait", Short: "Feishu: wait for more pictures or files before answering.", Group: GroupChatApps, Order: 3358, Advanced: true, Apply: Restart, Env: "ICEBATH_FEISHU_MEDIA_BATCH_DELAY_SECONDS"}, Default: 800 * time.Millisecond, Min: at(time.Duration(0))})

// Feishu: longest random wait added before reconnecting the socket
var FeishuWSReconnectNonce = Register(&Duration{Meta: Meta{Key: "feishu.ws_reconnect_nonce", Label: "Reconnect spread", Short: "Feishu: longest random wait added before the socket reconnects.", Group: GroupChatApps, Order: 3360, Advanced: true, Apply: Restart}, Default: 30 * time.Second, Min: at(time.Duration(0))})

// Feishu: wait between socket reconnect attempts
var FeishuWSReconnectInterval = Register(&Duration{Meta: Meta{Key: "feishu.ws_reconnect_interval", Label: "Reconnect wait", Short: "Feishu: wait between attempts to reconnect the socket.", Group: GroupChatApps, Order: 3362, Advanced: true, Apply: Restart}, Default: 2 * time.Minute, Min: at(time.Second)})

// Feishu: how often the socket is pinged
var FeishuWSPingInterval = Register(&Duration{Meta: Meta{Key: "feishu.ws_ping_interval", Label: "Ping interval", Short: "Feishu: how often the socket is pinged.", Group: GroupChatApps, Order: 3364, Advanced: true, Apply: Restart}, Null: "what the Feishu server asks for", Min: at(time.Second)})

// Feishu: how long a ping may go unanswered
var FeishuWSPingTimeout = Register(&Duration{Meta: Meta{Key: "feishu.ws_ping_timeout", Label: "Ping timeout", Short: "Feishu: how long a ping may go unanswered.", Group: GroupChatApps, Order: 3366, Advanced: true, Apply: Restart}, Null: "the default of the socket", Min: at(time.Second)})

// Weixin: who may message the bot directly
var WeixinDmPolicy = Register(&Enum{Meta: Meta{Key: "weixin.dm_policy", Env: "WEIXIN_DM_POLICY", Label: "Direct messages", Short: "Weixin: who may message the bot directly.", Group: GroupChatApps, Order: 3530, Advanced: true, Apply: Restart}, Default: "pairing", Open: true, Values: chatPolicyChoices})

// Weixin: people allowed to message the bot directly
var WeixinAllowFrom = Register(&List{Meta: Meta{Key: "weixin.allow_from", Env: "WEIXIN_ALLOWED_USERS", Label: "Allowed people", Short: "Weixin: user IDs allowed to message the bot directly.", Group: GroupChatApps, Order: 3540, Advanced: true, Apply: Restart}})

// Weixin: which group chats the bot answers in
var WeixinGroupPolicy = Register(&Enum{Meta: Meta{Key: "weixin.group_policy", Env: "WEIXIN_GROUP_POLICY", Label: "Group chats", Short: "Weixin: which group chats the bot answers in.", Group: GroupChatApps, Order: 3550, Advanced: true, Apply: Restart}, Default: "disabled", Open: true, Values: chatPolicyChoices})

// Weixin: group chats the bot answers in
var WeixinGroupAllowFrom = Register(&List{Meta: Meta{Key: "weixin.group_allow_from", Env: "WEIXIN_GROUP_ALLOWED_USERS", Label: "Allowed groups", Short: "Weixin: group chat IDs the bot answers in.", Group: GroupChatApps, Order: 3560, Advanced: true, Apply: Restart}})

// QQ: people allowed to message the bot directly
var QqbotAllowFrom = Register(&List{Meta: Meta{Key: "qqbot.allow_from", Env: "QQ_ALLOWED_USERS", Label: "Allowed people", Short: "QQ: user OpenIDs allowed to message the bot directly.", Group: GroupChatApps, Order: 3600, Advanced: true, Apply: Restart}})

// QQ: group chats the bot answers in
var QqbotGroupAllowFrom = Register(&List{Meta: Meta{Key: "qqbot.group_allow_from", Env: "QQ_GROUP_ALLOWED_USERS", Label: "Allowed groups", Short: "QQ: group OpenIDs the bot answers in.", Group: GroupChatApps, Order: 3610, Advanced: true, Apply: Restart}})

// QQ open to every sender
var QqbotAllowAllUsers = Register(&Bool{Meta: Meta{Key: "qqbot.allow_all_users", Label: "Allow everyone", Short: "QQ: let anyone message the bot.", Group: GroupChatApps, Order: 3615, Advanced: true, Env: "QQ_ALLOW_ALL_USERS"}, Default: false})

// QQ: who may message the bot directly
var QqbotDmPolicy = Register(&Enum{Meta: Meta{Key: "qqbot.dm_policy", Label: "Direct messages", Short: "QQ: who may message the bot directly.", Group: GroupChatApps, Order: 3620, Advanced: true, Apply: Restart}, Default: "pairing", Open: true, Values: chatPolicyChoices})

// QQ: which group chats the bot answers in
var QqbotGroupPolicy = Register(&Enum{Meta: Meta{Key: "qqbot.group_policy", Label: "Group chats", Short: "QQ: which group chats the bot answers in.", Group: GroupChatApps, Order: 3630, Advanced: true, Apply: Restart}, Default: "pairing", Open: true, Values: chatPolicyChoices})

// QQ replies sent as markdown
var QqbotMarkdownSupport = Register(&Bool{Meta: Meta{Key: "qqbot.markdown_support", Label: "Markdown replies", Short: "QQ: send replies as markdown when the bot is allowed to.", Group: GroupChatApps, Order: 3640, Advanced: true, Apply: Restart, Env: "QQ_MARKDOWN_SUPPORT"}, Default: true})

// QQ sandbox servers for a bot that is not published yet
var QqbotSandbox = Register(&Bool{Meta: Meta{Key: "qqbot.sandbox", Label: "Sandbox servers", Short: "QQ: use QQ's sandbox servers to test a bot before it goes live.", Group: GroupChatApps, Order: 3650, Advanced: true, Apply: Restart, Env: "QQ_SANDBOX"}, Default: false})

// Host of the QQ Open Platform portal setup talks to
var QqbotPortalHost = Register(&String{Meta: Meta{Key: "qqbot.portal_host", Label: "Portal host", Short: "QQ: host of the QQ Open Platform portal used during setup.", Group: GroupChatApps, Order: 3660, Advanced: true, Env: "QQ_PORTAL_HOST"}, Default: "q.qq.com"})

// QQ voice messages turned into text
var QqbotSttEnabled = Register(&Bool{Meta: Meta{Key: "qqbot.stt.enabled", Label: "Voice to text", Short: "QQ: turn voice messages into text when QQ sends none.", Help: "Needs the QQ_STT_API_KEY key.", Group: GroupChatApps, Order: 3670, Advanced: true}, Default: true})

// Service that turns QQ voice messages into text
var QqbotSttProvider = Register(&Enum{Meta: Meta{Key: "qqbot.stt.provider", Label: "Voice text service", Short: "QQ: service that turns voice messages into text.", Group: GroupChatApps, Order: 3680, Advanced: true}, Default: "zai",
	Values: []Choice{{"zai", "Z.AI", ""}, {"openai", "OpenAI", ""}}, Also: map[string]string{"glm": "zai"}})

// Address of your own service for QQ voice messages
var QqbotSttBaseURL = Register(&String{Meta: Meta{Key: "qqbot.stt.base_url", Label: "Voice text URL", Short: "QQ: address of your own voice to text service.", Help: "Empty uses the service picked above.", Group: GroupChatApps, Order: 3690, Advanced: true, Env: "QQ_STT_BASE_URL"}})

// Model that turns QQ voice messages into text
var QqbotSttModel = Register(&String{Meta: Meta{Key: "qqbot.stt.model", Label: "Voice text model", Short: "QQ: model that turns voice messages into text.", Help: "Empty uses the usual model of the service.", Group: GroupChatApps, Order: 3700, Advanced: true, Env: "QQ_STT_MODEL"}})

// Longest one QQ voice message may take to turn into text
var QqbotSttTimeout = Register(&Duration{Meta: Meta{Key: "qqbot.stt.timeout", Label: "Voice text limit", Short: "QQ: longest one voice message may take to turn into text.", Group: GroupChatApps, Order: 3710, Advanced: true}, Default: time.Minute, Min: at(time.Second)})

// Yuanbao: who may message the bot directly
var YuanbaoDmPolicy = Register(&Enum{Meta: Meta{Key: "yuanbao.dm_policy", Env: "YUANBAO_DM_POLICY", Label: "Direct messages", Short: "Yuanbao: who may message the bot directly.", Group: GroupChatApps, Order: 3930, Advanced: true, Apply: Restart}, Default: "pairing", Open: true, Values: chatPolicyChoices})

// Yuanbao: people allowed to message the bot directly
var YuanbaoDmAllowFrom = Register(&List{Meta: Meta{Key: "yuanbao.dm_allow_from", Env: "YUANBAO_DM_ALLOW_FROM", Label: "Allowed people", Short: "Yuanbao: user IDs allowed to message the bot directly.", Group: GroupChatApps, Order: 3940, Advanced: true, Apply: Restart}})

// Yuanbao: which group chats the bot answers in
var YuanbaoGroupPolicy = Register(&Enum{Meta: Meta{Key: "yuanbao.group_policy", Env: "YUANBAO_GROUP_POLICY", Label: "Group chats", Short: "Yuanbao: which group chats the bot answers in.", Group: GroupChatApps, Order: 3950, Advanced: true, Apply: Restart}, Default: "pairing", Open: true, Values: chatPolicyChoices})

// Yuanbao: group chats the bot answers in
var YuanbaoGroupAllowFrom = Register(&List{Meta: Meta{Key: "yuanbao.group_allow_from", Env: "YUANBAO_GROUP_ALLOW_FROM", Label: "Allowed groups", Short: "Yuanbao: group IDs the bot answers in.", Group: GroupChatApps, Order: 3960, Advanced: true, Apply: Restart}})

// Other icebath API servers this profile sends work to, by peer name
var BotPeers = Register(&Map{Meta: Meta{Key: "bot_peers", Label: "Peer bots", Short: "Other icebath API servers this profile can send work to, by name.", Help: "Managed with icebath peer add and icebath peer remove.", Group: GroupChatApps, Order: 3985, Advanced: true}})

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
var GatewayResetTriggers = Register(&List{Meta: Meta{Key: "gateway.reset_triggers", Label: "Reset words", Short: "Messages that start a new conversation in a chat app.", Help: "A message that is exactly one of these, in any letter case, works like /new.", Group: GroupChatApps, Order: 3997, Advanced: true}, Default: []any{"/new", "/reset"}})

// What happens when a stranger writes to the bot
var GatewayUnauthorizedDMBehavior = Register(&Enum{Meta: Meta{Key: "gateway.unauthorized_dm_behavior", Label: "Strangers", Short: "What the bot does when someone it does not know writes.", Group: GroupChatApps, Order: 3998, Advanced: true, Apply: Restart}, Default: "pair", Values: []Choice{{Value: "pair", Label: "Offer pairing", Meaning: "Reply with a pairing code."}, {Value: "ignore", Label: "Ignore", Meaning: "Do not reply."}, {Value: "decline", Label: "Decline", Meaning: "Reply with the decline message."}}})

// Reply sent to strangers when they are declined
var GatewayUnauthorizedDMDeclineMessage = Register(&String{Meta: Meta{Key: "gateway.unauthorized_dm_decline_message", Label: "Decline message", Short: "Reply sent to strangers when Strangers is Decline.", Group: GroupChatApps, Order: 3999, Advanced: true, Apply: Restart}})

// A2A: port the agent server listens on
var A2aPort = Register(&Int{Meta: Meta{Key: "a2a.port", Label: "A2A port", Short: "A2A: port the agent server listens on.", Group: GroupChatApps, Order: 900, Advanced: true, Apply: Restart, Env: "A2A_PORT"}, Default: 9900})

// A2A: address the agent server binds to
var A2aHost = Register(&String{Meta: Meta{Key: "a2a.host", Label: "A2A bind address", Short: "A2A: address the agent server binds to.", Help: "Any address other than this machine needs a bearer or peer token.", Group: GroupChatApps, Order: 901, Advanced: true, Apply: Restart, Env: "A2A_HOST"}, Default: "127.0.0.1"})

// A2A: accept tasks from every authenticated peer
var A2aAllowAllUsers = Register(&Bool{Meta: Meta{Key: "a2a.allow_all_users", Label: "Allow all peers", Short: "A2A: accept tasks from every peer that passes the token check.", Group: GroupChatApps, Order: 902, Advanced: true, Apply: Restart, Env: "A2A_ALLOW_ALL_USERS"}, Default: false})

// A2A: peer names allowed to send tasks
var A2aTrustedPeers = Register(&List{Meta: Meta{Key: "a2a.trusted_peers", Label: "Trusted peers", Short: "A2A: peer names allowed to send tasks.", Help: "Empty allows every peer on this machine and none from elsewhere.", Group: GroupChatApps, Order: 903, Advanced: true, Apply: Restart, Env: "A2A_TRUSTED_PEERS"}})

// A2A: name the agent card advertises
var A2aAgentName = Register(&String{Meta: Meta{Key: "a2a.agent_name", Label: "A2A agent name", Short: "A2A: name the agent card advertises.", Help: "Blank uses icebath- and the host name.", Group: GroupChatApps, Order: 904, Advanced: true, Apply: Restart, Env: "A2A_AGENT_NAME"}, Default: ""})

// A2A: description the agent card advertises
var A2aAgentDescription = Register(&String{Meta: Meta{Key: "a2a.agent_description", Label: "A2A description", Short: "A2A: description the agent card advertises.", Group: GroupChatApps, Order: 905, Advanced: true, Apply: Restart, Env: "A2A_AGENT_DESCRIPTION"}, Default: "Icebath Agent — a general-purpose agent reachable over A2A."})

// A2A: public address peers reach the agent server at
var A2aPublicURL = Register(&String{Meta: Meta{Key: "a2a.public_url", Label: "A2A public address", Short: "A2A: public address peers reach the agent server at.", Help: "Blank uses the address of each request.", Group: GroupChatApps, Order: 906, Advanced: true, Apply: Restart, Env: "A2A_PUBLIC_URL"}, Default: ""})

// A2A: toolsets the agent card lists as skills
var A2aAdvertisedToolsets = Register(&List{Meta: Meta{Key: "a2a.advertised_toolsets", Label: "Advertised toolsets", Short: "A2A: toolsets the agent card lists as skills.", Help: "Empty lists every toolset the agent has.", Group: GroupChatApps, Order: 907, Advanced: true, Apply: Restart, Env: "A2A_ADVERTISED_TOOLSETS"}})

// A2A: how long a peer task waits for the agent's answer
var A2aReplyTimeout = Register(&Duration{Meta: Meta{Key: "a2a.reply_timeout", Label: "A2A reply wait", Short: "A2A: how long a peer task waits for the agent's answer.", Help: "Less than one second counts as one second.", Group: GroupChatApps, Order: 908, Advanced: true, Apply: Restart, Env: "A2A_REPLY_TIMEOUT"}, Default: 300 * time.Second})

// A2A: organization the agent card names as its provider
var A2aProviderOrg = Register(&String{Meta: Meta{Key: "a2a.provider_org", Label: "A2A provider", Short: "A2A: organization the agent card names as its provider.", Group: GroupChatApps, Order: 909, Advanced: true, Env: "A2A_PROVIDER_ORG"}, Default: "Icebath Agent"})

// A2A: web address of the provider on the agent card
var A2aProviderURL = Register(&String{Meta: Meta{Key: "a2a.provider_url", Label: "A2A provider address", Short: "A2A: web address of the provider on the agent card.", Help: "Blank uses the agent's own address.", Group: GroupChatApps, Order: 910, Advanced: true, Env: "A2A_PROVIDER_URL"}, Default: ""})

// A2A: most inbound turns one peer conversation may take
var A2aMaxPingpongTurns = Register(&Int{Meta: Meta{Key: "a2a.max_pingpong_turns", Label: "A2A turn limit", Short: "A2A: most inbound turns one peer conversation may take.", Help: "Counted between 1 and 20.", Group: GroupChatApps, Order: 911, Advanced: true, Env: "A2A_MAX_PINGPONG_TURNS"}, Default: 5, Unit: "turns"})

// A2A: most requests one peer may send a minute
var A2aRateLimit = Register(&Int{Meta: Meta{Key: "a2a.rate_limit", Label: "A2A rate limit", Short: "A2A: most requests one peer may send a minute.", Help: "Less than one counts as one.", Group: GroupChatApps, Order: 912, Advanced: true, Apply: Restart, Env: "A2A_RATE_LIMIT"}, Default: 60, Unit: "requests"})

// A2A: other profiles served on their own path, keyed by slug
var A2aServedAgents = Register(&Map{Meta: Meta{Key: "a2a.served_agents", Label: "Served agents", Short: "A2A: other profiles served on their own path, keyed by slug.", Help: "Each entry may name profile, path, tenant, name, description, toolsets and timeout.", Group: GroupChatApps, Order: 913, Advanced: true, Apply: Restart}})

// A2A: peer agents the A2A tools can call, keyed by name
var A2aAgents = Register(&Map{Meta: Meta{Key: "a2a_agents", Label: "A2A peers", Short: "A2A: peer agents the A2A tools can call, keyed by name.", Help: "Each entry takes url, auth, timeout, capabilities and tenant.", Group: GroupChatApps, Order: 914, Advanced: true}})

// Email: how the connection that reads mail is secured
var EmailImapSecurity = Register(&Enum{Meta: Meta{Key: "email.imap_security", Label: "IMAP security", Short: "Email: how the connection that reads mail is secured.", Group: GroupChatApps, Order: 4000, Advanced: true, Apply: Restart, Env: "EMAIL_IMAP_SECURITY"}, Default: "tls", Values: []Choice{{Value: "tls", Label: "TLS", Meaning: "Encrypted from the first byte."}, {Value: "starttls", Label: "STARTTLS", Meaning: "Starts plain, then switches to encryption."}, {Value: "plain", Label: "None", Meaning: "No encryption."}}, Also: map[string]string{"ssl": "tls", "implicit": "tls", "start-tls": "starttls", "start_tls": "starttls", "none": "plain"}})

// Email: whether the certificate of the server mail is read from is checked
var EmailImapTLSVerify = Register(&Bool{Meta: Meta{Key: "email.imap_tls_verify", Label: "IMAP certificate check", Short: "Email: check the certificate of the server mail is read from.", Help: "Turn off only for a server you run yourself.", Group: GroupChatApps, Order: 4010, Advanced: true, Apply: Restart, Env: "EMAIL_IMAP_TLS_VERIFY"}, Default: true})

// Email: how the connection that sends mail is secured
var EmailSmtpSecurity = Register(&Enum{Meta: Meta{Key: "email.smtp_security", Label: "SMTP security", Short: "Email: how the connection that sends mail is secured.", Group: GroupChatApps, Order: 4020, Advanced: true, Apply: Restart, Env: "EMAIL_SMTP_SECURITY"}, Null: "STARTTLS, or TLS on port 465", Values: []Choice{{Value: "tls", Label: "TLS", Meaning: "Encrypted from the first byte."}, {Value: "starttls", Label: "STARTTLS", Meaning: "Starts plain, then switches to encryption."}, {Value: "plain", Label: "None", Meaning: "No encryption."}}, Also: map[string]string{"ssl": "tls", "implicit": "tls", "start-tls": "starttls", "start_tls": "starttls", "none": "plain"}})

// Email: whether the certificate of the server mail is sent through is checked
var EmailSmtpTLSVerify = Register(&Bool{Meta: Meta{Key: "email.smtp_tls_verify", Label: "SMTP certificate check", Short: "Email: check the certificate of the server mail is sent through.", Help: "Turn off only for a server you run yourself.", Group: GroupChatApps, Order: 4030, Advanced: true, Apply: Restart, Env: "EMAIL_SMTP_TLS_VERIFY"}, Default: true})

// Email: how often the mailbox is checked for new mail
var EmailPollInterval = Register(&Duration{Meta: Meta{Key: "email.poll_interval", Label: "Mail check interval", Short: "Email: how often the mailbox is checked for new mail.", Group: GroupChatApps, Order: 4040, Advanced: true, Apply: Restart, Env: "EMAIL_POLL_INTERVAL"}, Default: 15 * time.Second, Min: at(time.Second)})

// Email: attachments of incoming mail are left out
var EmailSkipAttachments = Register(&Bool{Meta: Meta{Key: "email.skip_attachments", Label: "Skip attachments", Short: "Email: leave the attachments of incoming mail out.", Group: GroupChatApps, Order: 4050, Advanced: true, Apply: Restart}, Default: false})

// Email: only mail whose sender the receiving server confirmed is answered
var EmailRequireAuthenticatedSender = Register(&Bool{Meta: Meta{Key: "email.require_authenticated_sender", Label: "Confirmed senders only", Short: "Email: answer only mail whose sender your mail server confirmed.", Help: "Off trusts the From line, which anyone can forge.", Group: GroupChatApps, Order: 4060, Advanced: true, Apply: Restart, Env: "EMAIL_TRUST_FROM_HEADER", EnvInvert: true}, Default: true})

// Email: name your mail server signs its sender checks with
var EmailAuthservID = Register(&String{Meta: Meta{Key: "email.authserv_id", Label: "Mail server ID", Short: "Email: name your mail server signs its sender checks with.", Help: "The authserv-id at the start of the top Authentication-Results header, such as mx.google.com.", Group: GroupChatApps, Order: 4070, Advanced: true, Apply: Restart, Env: "EMAIL_AUTHSERV_ID"}})

// Email: addresses allowed to write to the bot
var EmailAllowedUsers = Register(&List{Meta: Meta{Key: "email.allowed_users", Label: "Allowed senders", Short: "Email: addresses allowed to write to the bot.", Group: GroupChatApps, Order: 4080, Advanced: true, Env: "EMAIL_ALLOWED_USERS"}})

// Email: anyone may write to the bot
var EmailAllowAllUsers = Register(&Bool{Meta: Meta{Key: "email.allow_all_users", Label: "Allow every sender", Short: "Email: answer mail from any address.", Group: GroupChatApps, Order: 4090, Advanced: true, Env: "EMAIL_ALLOW_ALL_USERS"}, Default: false})

// Google Chat: people allowed to use the bot, by email or user name
var GoogleChatAllowedUsers = Register(&List{Meta: Meta{Key: "google_chat.allowed_users", Label: "Allowed users", Short: "Google Chat: people allowed to use the bot, by email or user name.", Help: "A star allows everyone.", Group: GroupChatApps, Order: 4100, Advanced: true, Env: "GOOGLE_CHAT_ALLOWED_USERS"}})

// Google Chat: anyone may use the bot
var GoogleChatAllowAllUsers = Register(&Bool{Meta: Meta{Key: "google_chat.allow_all_users", Label: "Allow everyone", Short: "Google Chat: let anyone use the bot.", Group: GroupChatApps, Order: 4110, Advanced: true, Env: "GOOGLE_CHAT_ALLOW_ALL_USERS"}, Default: false})

// Google Chat: messages fetched from the subscription per request
var GoogleChatMaxMessages = Register(&Int{Meta: Meta{Key: "google_chat.max_messages", Label: "Messages per fetch", Short: "Google Chat: messages fetched from the subscription per request.", Group: GroupChatApps, Order: 4120, Advanced: true, Apply: Restart, Env: "GOOGLE_CHAT_MAX_MESSAGES"}, Default: 1, Min: at(1), Unit: "messages"})

// Google Chat: size of the messages in work above which fetching waits
var GoogleChatMaxBytes = Register(&Int{Meta: Meta{Key: "google_chat.max_bytes", Label: "Work in hand", Short: "Google Chat: size of messages in work above which fetching waits.", Group: GroupChatApps, Order: 4130, Advanced: true, Apply: Restart, Env: "GOOGLE_CHAT_MAX_BYTES"}, Default: 16 * 1024 * 1024, Min: at(1), Unit: "bytes"})

// Google Chat: spaces searched for the user ID of the bot
var GoogleChatBootstrapSpaces = Register(&List{Meta: Meta{Key: "google_chat.bootstrap_spaces", Label: "Lookup spaces", Short: "Google Chat: spaces searched for the user ID of the bot.", Help: "The home space is searched first.", Group: GroupChatApps, Order: 4140, Advanced: true, Apply: Restart, Env: "GOOGLE_CHAT_BOOTSTRAP_SPACES"}})

// Google Chat: each event written to the debug log, secrets removed
var GoogleChatDebugRaw = Register(&Bool{Meta: Meta{Key: "google_chat.debug_raw", Label: "Log raw events", Short: "Google Chat: write each event to the debug log, secrets removed.", Group: GroupChatApps, Order: 4150, Advanced: true, Apply: Restart, Env: "GOOGLE_CHAT_DEBUG_RAW"}, Default: false})

// Home Assistant: kinds of device whose changes reach the agent
var HomeassistantWatchDomains = Register(&List{Meta: Meta{Key: "homeassistant.watch_domains", Label: "Watched kinds", Short: "Home Assistant: kinds of device whose changes reach the agent.", Help: "The part of an entity ID before the dot, such as light or climate.", Group: GroupChatApps, Order: 4200, Advanced: true, Apply: Restart}})

// Home Assistant: single entities whose changes reach the agent
var HomeassistantWatchEntities = Register(&List{Meta: Meta{Key: "homeassistant.watch_entities", Label: "Watched entities", Short: "Home Assistant: single entities whose changes reach the agent.", Group: GroupChatApps, Order: 4210, Advanced: true, Apply: Restart}})

// Home Assistant: entities whose changes never reach the agent
var HomeassistantIgnoreEntities = Register(&List{Meta: Meta{Key: "homeassistant.ignore_entities", Label: "Ignored entities", Short: "Home Assistant: entities whose changes never reach the agent.", Group: GroupChatApps, Order: 4220, Advanced: true, Apply: Restart}})

// Home Assistant: every change reaches the agent when nothing is watched by name
var HomeassistantWatchAll = Register(&Bool{Meta: Meta{Key: "homeassistant.watch_all", Label: "Watch everything", Short: "Home Assistant: send every change when nothing is watched by name.", Group: GroupChatApps, Order: 4230, Advanced: true, Apply: Restart}, Default: false})

// Home Assistant: least time between two reports about one entity
var HomeassistantCooldown = Register(&Duration{Meta: Meta{Key: "homeassistant.cooldown_seconds", Label: "Report pause", Short: "Home Assistant: least time between two reports about one entity.", Group: GroupChatApps, Order: 4240, Advanced: true, Apply: Restart}, Default: 30 * time.Second, Min: at(time.Duration(0))})

// IRC: the connection to the server is encrypted
var IrcUseTLS = Register(&Bool{Meta: Meta{Key: "irc.use_tls", Label: "Encrypt connection", Short: "IRC: encrypt the connection to the server.", Group: GroupChatApps, Order: 4300, Advanced: true, Apply: Restart, Env: "IRC_USE_TLS"}, Default: true})

// IRC: nicknames allowed to talk to the bot
var IrcAllowedUsers = Register(&List{Meta: Meta{Key: "irc.allowed_users", Label: "Allowed nicknames", Short: "IRC: nicknames allowed to talk to the bot.", Help: "Letter case does not matter.", Group: GroupChatApps, Order: 4310, Advanced: true, Apply: Restart, Env: "IRC_ALLOWED_USERS"}})

// IRC: anyone may talk to the bot
var IrcAllowAllUsers = Register(&Bool{Meta: Meta{Key: "irc.allow_all_users", Label: "Allow everyone", Short: "IRC: let anyone talk to the bot.", Group: GroupChatApps, Order: 4320, Advanced: true, Env: "IRC_ALLOW_ALL_USERS"}, Default: false})

// IRC: longest line sent before a reply is split
var IrcMaxMessageLength = Register(&Int{Meta: Meta{Key: "irc.max_message_length", Label: "Line length", Short: "IRC: longest line sent before a reply is split.", Group: GroupChatApps, Order: 4330, Advanced: true, Apply: Restart}, Default: 450, Min: at(1), Unit: "characters"})

// LINE: address the message receiver listens on
var LineHost = Register(&String{Meta: Meta{Key: "line.host", Label: "Listen address", Short: "LINE: address the message receiver listens on.", Group: GroupChatApps, Order: 4400, Advanced: true, Apply: Restart, Env: "LINE_HOST"}, Null: "every address of this computer"})

// LINE: port the message receiver listens on
var LinePort = Register(&Int{Meta: Meta{Key: "line.port", Label: "Listen port", Short: "LINE: port the message receiver listens on.", Group: GroupChatApps, Order: 4410, Advanced: true, Apply: Restart, Env: "LINE_PORT"}, Default: 8646, Min: at(1), Max: at(65535)})

// LINE: path the message receiver answers on
var LineWebhookPath = Register(&String{Meta: Meta{Key: "line.webhook_path", Label: "Webhook path", Short: "LINE: path the message receiver answers on.", Group: GroupChatApps, Order: 4420, Advanced: true, Apply: Restart}, Default: "/line/webhook"})

// LINE: public address LINE reaches the message receiver at
var LinePublicURL = Register(&String{Meta: Meta{Key: "line.public_url", Label: "Public address", Short: "LINE: public address LINE reaches the message receiver at.", Help: "Pictures and files are handed to LINE as links under it.", Group: GroupChatApps, Order: 4430, Advanced: true, Apply: Restart, Env: "LINE_PUBLIC_URL"}})

// LINE: anyone may talk to the bot
var LineAllowAllUsers = Register(&Bool{Meta: Meta{Key: "line.allow_all_users", Label: "Allow everyone", Short: "LINE: let anyone talk to the bot.", Group: GroupChatApps, Order: 4440, Advanced: true, Apply: Restart, Env: "LINE_ALLOW_ALL_USERS"}, Default: false})

// LINE: user IDs allowed to talk to the bot
var LineAllowedUsers = Register(&List{Meta: Meta{Key: "line.allowed_users", Label: "Allowed users", Short: "LINE: user IDs allowed to talk to the bot.", Group: GroupChatApps, Order: 4450, Advanced: true, Apply: Restart, Env: "LINE_ALLOWED_USERS"}})

// LINE: group IDs the bot answers in
var LineAllowedGroups = Register(&List{Meta: Meta{Key: "line.allowed_groups", Label: "Allowed groups", Short: "LINE: group IDs the bot answers in.", Group: GroupChatApps, Order: 4460, Advanced: true, Apply: Restart, Env: "LINE_ALLOWED_GROUPS"}})

// LINE: room IDs the bot answers in
var LineAllowedRooms = Register(&List{Meta: Meta{Key: "line.allowed_rooms", Label: "Allowed rooms", Short: "LINE: room IDs the bot answers in.", Group: GroupChatApps, Order: 4470, Advanced: true, Apply: Restart, Env: "LINE_ALLOWED_ROOMS"}})

// LINE: how long a reply may take before a button to fetch it is sent
var LineSlowResponseThreshold = Register(&Duration{Meta: Meta{Key: "line.slow_response_threshold", Label: "Slow reply wait", Short: "LINE: wait before a button to fetch a slow reply is sent.", Help: "Zero never sends the button.", Group: GroupChatApps, Order: 4480, Advanced: true, Apply: Restart, Env: "LINE_SLOW_RESPONSE_THRESHOLD"}, Default: 45 * time.Second, Min: at(time.Duration(0))})

// LINE: message sent with the button when a reply is slow
var LinePendingText = Register(&String{Meta: Meta{Key: "line.pending_text", Label: "Slow reply text", Short: "LINE: message sent with the button when a reply is slow.", Group: GroupChatApps, Order: 4490, Advanced: true, Apply: Restart, Env: "LINE_PENDING_TEXT"}, Null: "the built-in text in your language"})

// LINE: label of the button that fetches a slow reply
var LineButtonLabel = Register(&String{Meta: Meta{Key: "line.button_label", Label: "Fetch button label", Short: "LINE: label of the button that fetches a slow reply.", Group: GroupChatApps, Order: 4500, Advanced: true, Apply: Restart, Env: "LINE_BUTTON_LABEL"}, Null: "the built-in text in your language"})

// LINE: answer when the button is pressed after the reply was delivered
var LineDeliveredText = Register(&String{Meta: Meta{Key: "line.delivered_text", Label: "Already sent text", Short: "LINE: answer when the button is pressed after the reply was sent.", Group: GroupChatApps, Order: 4510, Advanced: true, Apply: Restart, Env: "LINE_DELIVERED_TEXT"}, Null: "the built-in text in your language"})

// LINE: answer when the button is pressed after the turn was stopped
var LineInterruptedText = Register(&String{Meta: Meta{Key: "line.interrupted_text", Label: "Stopped turn text", Short: "LINE: answer when the button is pressed after the turn was stopped.", Group: GroupChatApps, Order: 4520, Advanced: true, Apply: Restart, Env: "LINE_INTERRUPTED_TEXT"}, Null: "the built-in text in your language"})

// LINE: answer when the button is pressed after the reply was dropped
var LineExpiredText = Register(&String{Meta: Meta{Key: "line.expired_text", Label: "Lost reply text", Short: "LINE: answer when the button is pressed after the reply was dropped.", Group: GroupChatApps, Order: 4530, Advanced: true, Apply: Restart, Env: "LINE_EXPIRED_TEXT"}, Null: "the built-in text in your language"})

// ntfy: notifications are sent as markdown
var NtfyMarkdown = Register(&Bool{Meta: Meta{Key: "ntfy.markdown", Label: "Markdown", Short: "ntfy: send notifications as markdown.", Group: GroupChatApps, Order: 4600, Advanced: true, Env: "NTFY_MARKDOWN"}, Default: false})

// ntfy: sender names allowed to message the bot
var NtfyAllowedUsers = Register(&List{Meta: Meta{Key: "ntfy.allowed_users", Label: "Allowed senders", Short: "ntfy: sender names allowed to message the bot.", Group: GroupChatApps, Order: 4610, Advanced: true, Env: "NTFY_ALLOWED_USERS"}})

// ntfy: anyone who can publish to the topic may message the bot
var NtfyAllowAllUsers = Register(&Bool{Meta: Meta{Key: "ntfy.allow_all_users", Label: "Allow everyone", Short: "ntfy: let anyone who can publish to the topic message the bot.", Group: GroupChatApps, Order: 4620, Advanced: true, Env: "NTFY_ALLOW_ALL_USERS"}, Default: false})

// Raft: address the wake listener binds to
var RaftHost = Register(&String{Meta: Meta{Key: "raft.host", Label: "Listener address", Short: "Raft: address the listener for wake hints binds to.", Group: GroupChatApps, Order: 4700, Advanced: true, Apply: Restart}, Default: "127.0.0.1"})

// Raft: port the wake listener binds to, zero for any free port
var RaftPort = Register(&Int{Meta: Meta{Key: "raft.port", Label: "Listener port", Short: "Raft: port the listener for wake hints binds to.", Help: "Zero takes any free port.", Group: GroupChatApps, Order: 4710, Advanced: true, Apply: Restart}, Default: 0, Min: at(0), Max: at(65535)})

// Raft: path wake hints are sent to
var RaftPath = Register(&String{Meta: Meta{Key: "raft.path", Label: "Listener path", Short: "Raft: path the bridge sends wake hints to.", Group: GroupChatApps, Order: 4720, Advanced: true, Apply: Restart}, Default: "/wake"})

// Raft: name of the session wake hints belong to
var RaftRuntimeSession = Register(&String{Meta: Meta{Key: "raft.runtime_session", Label: "Session name", Short: "Raft: name of the session wake hints belong to.", Group: GroupChatApps, Order: 4730, Advanced: true, Apply: Restart}, Default: "default"})

// Raft: largest wake hint the listener takes
var RaftMaxBodyBytes = Register(&Int{Meta: Meta{Key: "raft.max_body_bytes", Label: "Hint size limit", Short: "Raft: largest wake hint the listener takes.", Group: GroupChatApps, Order: 4740, Advanced: true, Apply: Restart}, Default: 16384, Min: at(1), Unit: "bytes"})

// SMS: address the message receiver listens on
var SmsWebhookHost = Register(&String{Meta: Meta{Key: "sms.webhook_host", Label: "Listen address", Short: "SMS: address the message receiver listens on.", Group: GroupChatApps, Order: 4800, Advanced: true, Apply: Restart, Env: "SMS_WEBHOOK_HOST"}, Default: "127.0.0.1"})

// SMS: port the message receiver listens on
var SmsWebhookPort = Register(&Int{Meta: Meta{Key: "sms.webhook_port", Label: "Listen port", Short: "SMS: port the message receiver listens on.", Group: GroupChatApps, Order: 4810, Advanced: true, Apply: Restart, Env: "SMS_WEBHOOK_PORT"}, Default: 8080, Min: at(1), Max: at(65535)})

// SMS: public address Twilio sends messages to, which its signature is checked against
var SmsWebhookURL = Register(&String{Meta: Meta{Key: "sms.webhook_url", Label: "Public address", Short: "SMS: public address Twilio sends messages to.", Help: "The exact URL set in your Twilio console; the signature of each message is checked against it.", Group: GroupChatApps, Order: 4820, Advanced: true, Apply: Restart, Env: "SMS_WEBHOOK_URL"}})

// SMS: the signature of each incoming message is checked
var SmsVerifySignature = Register(&Bool{Meta: Meta{Key: "sms.verify_signature", Label: "Check signature", Short: "SMS: check that each incoming message really comes from Twilio.", Help: "Off lets anyone who reaches the listen port inject messages.", Group: GroupChatApps, Order: 4830, Advanced: true, Apply: Restart, Env: "SMS_INSECURE_NO_SIGNATURE", EnvInvert: true}, Default: true})

// SMS: phone numbers allowed to text the bot
var SmsAllowedUsers = Register(&List{Meta: Meta{Key: "sms.allowed_users", Label: "Allowed numbers", Short: "SMS: phone numbers allowed to text the bot.", Group: GroupChatApps, Order: 4840, Advanced: true, Env: "SMS_ALLOWED_USERS"}})

// SMS: anyone may text the bot
var SmsAllowAllUsers = Register(&Bool{Meta: Meta{Key: "sms.allow_all_users", Label: "Allow everyone", Short: "SMS: let any phone number text the bot.", Group: GroupChatApps, Order: 4850, Advanced: true, Env: "SMS_ALLOW_ALL_USERS"}, Default: false})

// SimpleX: contact requests are accepted without asking
var SimplexAutoAccept = Register(&Bool{Meta: Meta{Key: "simplex.auto_accept", Label: "Accept contacts", Short: "SimpleX: accept contact requests without asking.", Group: GroupChatApps, Order: 4900, Advanced: true, Apply: Restart, Env: "SIMPLEX_AUTO_ACCEPT"}, Default: true})

// SimpleX: group IDs the bot answers in
var SimplexGroupAllowed = Register(&List{Meta: Meta{Key: "simplex.group_allowed", Label: "Allowed groups", Short: "SimpleX: group IDs the bot answers in.", Help: "Empty answers in no group.", Group: GroupChatApps, Order: 4910, Advanced: true, Apply: Restart, Env: "SIMPLEX_GROUP_ALLOWED"}})

// SimpleX: contact IDs allowed to message the bot
var SimplexAllowedUsers = Register(&List{Meta: Meta{Key: "simplex.allowed_users", Label: "Allowed contacts", Short: "SimpleX: contact IDs allowed to message the bot.", Help: "Numeric contact IDs, see /contacts; display names are not trusted.", Group: GroupChatApps, Order: 4920, Advanced: true, Env: "SIMPLEX_ALLOWED_USERS"}})

// SimpleX: anyone may message the bot
var SimplexAllowAllUsers = Register(&Bool{Meta: Meta{Key: "simplex.allow_all_users", Label: "Allow everyone", Short: "SimpleX: let any contact message the bot.", Group: GroupChatApps, Order: 4930, Advanced: true, Env: "SIMPLEX_ALLOW_ALL_USERS"}, Default: false})

// SimpleX: wait for more text before answering a message sent in parts
var SimplexTextBatchDelay = Register(&Duration{Meta: Meta{Key: "simplex.text_batch_delay_seconds", Label: "Text gather wait", Short: "SimpleX: wait for more text before answering a message sent in parts.", Group: GroupChatApps, Order: 4940, Advanced: true, Apply: Restart, Env: "ICEBATH_SIMPLEX_TEXT_BATCH_DELAY"}, Default: 800 * time.Millisecond, Min: at(time.Duration(0))})

// Teams: address the message receiver listens on
var TeamsHost = Register(&String{Meta: Meta{Key: "teams.host", Label: "Listen address", Short: "Teams: address the message receiver listens on.", Group: GroupChatApps, Order: 5000, Advanced: true, Apply: Restart, Env: "TEAMS_HOST"}, Null: "every address of this computer"})

// Teams: port the message receiver listens on
var TeamsPort = Register(&Int{Meta: Meta{Key: "teams.port", Label: "Listen port", Short: "Teams: port the message receiver listens on.", Group: GroupChatApps, Order: 5010, Advanced: true, Apply: Restart, Env: "TEAMS_PORT"}, Default: 3978, Min: at(1), Max: at(65535)})

// Teams: only answer in channels and group chats when mentioned
var TeamsRequireMention = Register(&Bool{Meta: Meta{Key: "teams.require_mention", Label: "Require mention", Short: "Teams: only answer in channels and group chats when mentioned.", Group: GroupChatApps, Order: 5020, Advanced: true, Apply: Restart, Env: "TEAMS_REQUIRE_MENTION"}, Default: false})

// Teams: user IDs allowed to talk to the bot
var TeamsAllowedUsers = Register(&List{Meta: Meta{Key: "teams.allowed_users", Label: "Allowed users", Short: "Teams: user IDs allowed to talk to the bot.", Help: "The Microsoft Entra object ID of each user. A star allows everyone.", Group: GroupChatApps, Order: 5030, Advanced: true, Env: "TEAMS_ALLOWED_USERS"}})

// Teams: anyone may talk to the bot
var TeamsAllowAllUsers = Register(&Bool{Meta: Meta{Key: "teams.allow_all_users", Label: "Allow everyone", Short: "Teams: let anyone talk to the bot.", Group: GroupChatApps, Order: 5040, Advanced: true, Env: "TEAMS_ALLOW_ALL_USERS"}, Default: false})
