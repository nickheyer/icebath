package config

import (
	"reflect"

	"gopkg.in/yaml.v3"
)

// Messaging gateway section, with the nested forms of the root gateway keys and platform shorthand blocks kept by name
type GatewaySection struct {
	UnmanagedIdleExitSeconds      float64               `yaml:"unmanaged_idle_exit_seconds"`
	SignalInterruptGraceTimeout   float64               `yaml:"signal_interrupt_grace_timeout"`
	ServiceInstallChoice          *ServiceInstallChoice `yaml:"service_install_choice"`
	DeliveryLedger                bool                  `yaml:"delivery_ledger" coerce:"boolwords"`
	PlatformConnectTimeout        float64               `yaml:"platform_connect_timeout"`
	LoopWatchdog                  bool                  `yaml:"loop_watchdog"`
	LoopWatchdogProbeIntervalS    float64               `yaml:"loop_watchdog_probe_interval_s"`
	LoopWatchdogProbeTimeoutS     float64               `yaml:"loop_watchdog_probe_timeout_s"`
	LoopWatchdogMaxStrikes        int                   `yaml:"loop_watchdog_max_strikes"`
	AllowAllUsers                 bool                  `yaml:"allow_all_users" coerce:"strset"`
	BotLoopGuard                  BotLoopGuard          `yaml:"bot_loop_guard"`
	StartupWatchdog               bool                  `yaml:"startup_watchdog" coerce:"bool"`
	StartupWatchdogTimeoutSeconds float64               `yaml:"startup_watchdog_timeout_seconds"`
	WriteSessionsJSON             bool                  `yaml:"write_sessions_json"`
	MultiplexProfiles             Scalar                `yaml:"multiplex_profiles"`
	AutoMultiplexMigration        bool                  `yaml:"auto_multiplex_migration"`
	ProfileRoutes                 []ProfileRoute        `yaml:"profile_routes"`
	ScaleToZero                   ScaleToZero           `yaml:"scale_to_zero"`
	RestartLoopGuard              RestartLoopGuard      `yaml:"restart_loop_guard"`
	RespawnStorm                  RespawnStorm          `yaml:"respawn_storm"`
	MessageTimestamps             MessageTimestamps     `yaml:"message_timestamps"`
	MaxInboundMediaBytes          int64                 `yaml:"max_inbound_media_bytes"`
	TrustEnv                      bool                  `yaml:"trust_env" coerce:"notoff"`
	Strict                        bool                  `yaml:"strict" coerce:"bool"`
	MediaDeliveryAllowDirs        StringList            `yaml:"media_delivery_allow_dirs"`
	TrustRecentFiles              bool                  `yaml:"trust_recent_files" coerce:"bool"`
	TrustRecentFilesSeconds       float64               `yaml:"trust_recent_files_seconds"`
	APIServer                     APIServerSettings     `yaml:"api_server"`
	Nested                        gatewayNested         `yaml:",inline"`
	rest                          rest
}

// Root gateway keys also accepted nested under gateway, each used only when the root form is absent
type gatewayNested struct {
	Platforms                    Map[PlatformBlock] `yaml:"platforms,omitempty"`
	QuickCommands                Map[QuickCommand]  `yaml:"quick_commands,omitempty"`
	STT                          Map[Raw]           `yaml:"stt,omitempty"`
	STTEchoTranscripts           Scalar             `yaml:"stt_echo_transcripts,omitempty"`
	GroupSessionsPerUser         Scalar             `yaml:"group_sessions_per_user,omitempty"`
	ThreadSessionsPerUser        Scalar             `yaml:"thread_sessions_per_user,omitempty"`
	RoomLinkURL                  string             `yaml:"room_link_url,omitempty" coerce:"exact"`
	MaxConcurrentSessions        Scalar             `yaml:"max_concurrent_sessions,omitempty"`
	MaxLiveSessions              Scalar             `yaml:"max_live_sessions,omitempty"`
	SystemdWatchdogSeconds       Scalar             `yaml:"systemd_watchdog_seconds,omitempty"`
	Streaming                    *StreamingConfig   `yaml:"streaming,omitempty"`
	ResetTriggers                []string           `yaml:"reset_triggers,omitempty"`
	AlwaysLogLocal               Scalar             `yaml:"always_log_local,omitempty"`
	FilterSilenceNarration       Scalar             `yaml:"filter_silence_narration,omitempty"`
	UnauthorizedDMBehavior       string             `yaml:"unauthorized_dm_behavior,omitempty"`
	UnauthorizedDMDeclineMessage string             `yaml:"unauthorized_dm_decline_message,omitempty"`
	OnAllAdaptersDown            string             `yaml:"on_all_adapters_down,omitempty"`
	RelayURL                     string             `yaml:"relay_url,omitempty"`
}

type gatewayFields GatewaySection

// Decodes the mapping and keeps platform shorthand blocks and unknown keys
func (g *GatewaySection) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*gatewayFields)(g), &g.rest)
}

// Writes the mapping with shorthand blocks after the named ones
func (g GatewaySection) MarshalYAML() (any, error) {
	return encodeMapping(gatewayFields(g), g.rest)
}

// Platform block written as gateway.<platform> shorthand, the api_server settings among them, ok false when none is held
func (g GatewaySection) Shorthand(platform string) (PlatformBlock, bool, error) {
	if platform == "api_server" {
		a := g.APIServer
		if isZero(reflect.ValueOf(a)) && len(a.rest.pairs) == 0 && len(a.rest.held) == 0 {
			return PlatformBlock{}, false, nil
		}
		b, err := blockView(g.APIServer)
		return b, true, err
	}
	n := g.rest.get(platform)
	if n == nil || n.Kind != yaml.MappingNode {
		return PlatformBlock{}, false, nil
	}
	var b PlatformBlock
	err := b.UnmarshalYAML(n)
	return b, true, err
}

// Unknown gateway keys holding a mapping in file order, the candidate gateway.<platform> shorthand blocks besides api_server
func (g GatewaySection) ShorthandNames() []string {
	var out []string
	for _, k := range g.rest.names() {
		if n := g.rest.get(k); n != nil && n.Kind == yaml.MappingNode {
			out = append(out, k)
		}
	}
	return out
}

// Recorded answer to the gateway service install offer
type ServiceInstallChoice string

// Service install answers
const (
	ServiceInstall ServiceInstallChoice = "install"
	ServiceDecline ServiceInstallChoice = "decline"
)

// Accepted service install answers, null meaning not asked yet
func ServiceInstallChoices() []ServiceInstallChoice {
	return []ServiceInstallChoice{ServiceInstall, ServiceDecline}
}

// Behaviors when the last messaging adapter goes down
func AllAdaptersDownPolicies() []string {
	return []string{"exit", "stay_alive"}
}

// Bot-to-bot loop guard
type BotLoopGuard struct {
	Enabled         bool    `yaml:"enabled"`
	MaxEvents       int     `yaml:"max_events" coerce:"whole"`
	WindowSeconds   float64 `yaml:"window_seconds" coerce:"strict"`
	CooldownSeconds float64 `yaml:"cooldown_seconds" coerce:"strict"`
	rest            rest
}

type botLoopGuardFields BotLoopGuard

// Decodes the mapping and keeps unknown keys
func (b *BotLoopGuard) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*botLoopGuardFields)(b), &b.rest)
}

// Writes the mapping with unknown keys after the named ones
func (b BotLoopGuard) MarshalYAML() (any, error) {
	return encodeMapping(botLoopGuardFields(b), b.rest)
}

// Route of default-profile inbound chats to another profile, user_id kept as written since null or empty voids the route
type ProfileRoute struct {
	Name       string `yaml:"name,omitempty"`
	Profile    string `yaml:"profile,omitempty"`
	Platform   string `yaml:"platform,omitempty"`
	GuildID    string `yaml:"guild_id,omitempty"`
	ChatID     string `yaml:"chat_id,omitempty"`
	ThreadID   string `yaml:"thread_id,omitempty"`
	UserID     Scalar `yaml:"user_id,omitempty"`
	Enabled    *bool  `yaml:"enabled,omitempty" coerce:"bool"`
	BotProfile string `yaml:"bot_profile,omitempty"`
	raw        *yaml.Node
	rest       rest
}

type profileRouteFields ProfileRoute

// Decodes a mapping route keeping other selector keys, and holds a stray non-mapping item as written
func (p *ProfileRoute) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n == nil || n.Kind != yaml.MappingNode {
		*p = ProfileRoute{raw: plainCopy(n)}
		return nil
	}
	p.raw = nil
	return decodeEntry(n, (*profileRouteFields)(p), &p.rest)
}

// Keeps an explicit null as a stray item
func (ProfileRoute) holdsNull() {}

// Writes the route, or the stray item exactly as read
func (p ProfileRoute) MarshalYAML() (any, error) {
	if p.raw != nil {
		return emitted(p.raw), nil
	}
	return encodeMapping(profileRouteFields(p), p.rest)
}

// Reports a mapping route rather than a stray scalar or list item
func (p ProfileRoute) Valid() bool {
	return p.raw == nil
}

// Scale-to-zero idle timeout
type ScaleToZero struct {
	IdleTimeoutMinutes float64 `yaml:"idle_timeout_minutes"`
	rest               rest
}

type scaleToZeroFields ScaleToZero

// Decodes the mapping and keeps unknown keys
func (s *ScaleToZero) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*scaleToZeroFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s ScaleToZero) MarshalYAML() (any, error) {
	return encodeMapping(scaleToZeroFields(s), s.rest)
}

// Auto-resume restart-loop breaker
type RestartLoopGuard struct {
	MaxRestarts   int `yaml:"max_restarts" coerce:"exactint"`
	WindowSeconds int `yaml:"window_seconds" coerce:"exactint"`
	MaxGapSeconds int `yaml:"max_gap_seconds" coerce:"exactint"`
	rest          rest
}

type restartLoopGuardFields RestartLoopGuard

// Decodes the mapping and keeps unknown keys
func (r *RestartLoopGuard) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*restartLoopGuardFields)(r), &r.rest)
}

// Writes the mapping with unknown keys after the named ones
func (r RestartLoopGuard) MarshalYAML() (any, error) {
	return encodeMapping(restartLoopGuardFields(r), r.rest)
}

// Respawn-storm circuit breaker
type RespawnStorm struct {
	MaxStarts     int     `yaml:"max_starts" coerce:"exactint"`
	WindowSeconds float64 `yaml:"window_seconds" coerce:"exact"`
	rest          rest
}

type respawnStormFields RespawnStorm

// Decodes the mapping and keeps unknown keys
func (r *RespawnStorm) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*respawnStormFields)(r), &r.rest)
}

// Writes the mapping with unknown keys after the named ones
func (r RespawnStorm) MarshalYAML() (any, error) {
	return encodeMapping(respawnStormFields(r), r.rest)
}

// Timestamp prefix on user messages in model context, also written as a bare truthy scalar
type MessageTimestamps struct {
	Enabled bool `yaml:"enabled" coerce:"bool"`
	bare    bareForm
	rest    rest
}

type messageTimestampsFields MessageTimestamps

// Decodes the mapping and keeps unknown keys, a bare scalar standing for enabled by its truthiness
func (m *MessageTimestamps) UnmarshalYAML(n *yaml.Node) error {
	return decodeSwitchable(n, (*messageTimestampsFields)(m), &m.rest, &m.bare, func(s *yaml.Node) {
		var on bool
		if scalarInto(s, reflect.ValueOf(&on).Elem(), coerceBoolOf) == nil {
			m.Enabled = on
		}
	})
}

// Writes the bare scalar the file held while nothing changed, else the mapping with unknown keys last
func (m MessageTimestamps) MarshalYAML() (any, error) {
	return encodeSwitchable(messageTimestampsFields(m), m.rest, m.bare)
}

// OpenAI-compatible API server platform limits
type APIServerSettings struct {
	MaxConcurrentRuns         int `yaml:"max_concurrent_runs"`
	HistoryToolOutputMaxChars int `yaml:"history_tool_output_max_chars"`
	rest                      rest
}

type apiServerFields APIServerSettings

// Decodes the mapping and keeps adapter keys
func (a *APIServerSettings) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*apiServerFields)(a), &a.rest)
}

// Writes the mapping with adapter keys after the named ones
func (a APIServerSettings) MarshalYAML() (any, error) {
	return encodeMapping(apiServerFields(a), a.rest)
}

// Real-time token streaming to messaging platforms, with mode as the older transport alias that implies enabled
type StreamingConfig struct {
	Enabled                Scalar          `yaml:"enabled"`
	Transport              StreamTransport `yaml:"transport"`
	EditInterval           float64         `yaml:"edit_interval"`
	BufferThreshold        int             `yaml:"buffer_threshold"`
	Cursor                 string          `yaml:"cursor"`
	FreshFinalAfterSeconds float64         `yaml:"fresh_final_after_seconds"`
	Mode                   string          `yaml:"mode,omitempty"`
	rest                   rest
}

type streamingFields StreamingConfig

// Decodes the mapping and keeps unknown keys
func (s *StreamingConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*streamingFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s StreamingConfig) MarshalYAML() (any, error) {
	return encodeMapping(streamingFields(s), s.rest)
}

// Streaming delivery transport
type StreamTransport string

// Streaming transports
const (
	StreamAuto  StreamTransport = "auto"
	StreamDraft StreamTransport = "draft"
	StreamEdit  StreamTransport = "edit"
	StreamOff   StreamTransport = "off"
)

// Accepted streaming transports, read case-insensitively with true as auto, false as off and null or empty as auto
func StreamTransports() []StreamTransport {
	return []StreamTransport{StreamAuto, StreamDraft, StreamEdit, StreamOff}
}

// Session history maintenance section, auto_prune and orphan_end_grace_s pointers since their readers act on absence
type SessionsConfig struct {
	AutoPrune             *bool       `yaml:"auto_prune" coerce:"bool"`
	RetentionDays         int         `yaml:"retention_days"`
	AutoArchive           bool        `yaml:"auto_archive"`
	AutoArchiveDays       float64     `yaml:"auto_archive_days"`
	ShowSubagents         bool        `yaml:"show_subagents"`
	VacuumAfterPrune      bool        `yaml:"vacuum_after_prune" coerce:"bool"`
	MinVacuumIntervalDays int         `yaml:"min_vacuum_interval_days"`
	MinIntervalHours      int         `yaml:"min_interval_hours"`
	FTSOptimizeNotice     NoticeLevel `yaml:"fts_optimize_notice"`
	CJKFTS                bool        `yaml:"cjk_fts"`
	SearchSlowMS          int         `yaml:"search_slow_ms"`
	MaxResumeMessages     int         `yaml:"max_resume_messages"`
	MaxExportMessages     int         `yaml:"max_export_messages"`
	OrphanEndGraceS       *float64    `yaml:"orphan_end_grace_s"`
	rest                  rest
}

type sessionsFields SessionsConfig

// Decodes the mapping and keeps unknown keys
func (s *SessionsConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*sessionsFields)(s), &s.rest)
}

// Writes the mapping with unknown keys after the named ones
func (s SessionsConfig) MarshalYAML() (any, error) {
	return encodeMapping(sessionsFields(s), s.rest)
}

// Whether a maintenance notice is printed and how strongly
type NoticeLevel string

// Notice levels
const (
	NoticeAdvise  NoticeLevel = "advise"
	NoticeRequire NoticeLevel = "require"
	NoticeOff     NoticeLevel = "off"
)

// Accepted notice levels
func NoticeLevels() []NoticeLevel {
	return []NoticeLevel{NoticeAdvise, NoticeRequire, NoticeOff}
}

// Defaults of the gateway section
func defaultGateway() GatewaySection {
	return GatewaySection{
		UnmanagedIdleExitSeconds:      600,
		SignalInterruptGraceTimeout:   1,
		DeliveryLedger:                true,
		PlatformConnectTimeout:        30,
		LoopWatchdog:                  true,
		LoopWatchdogProbeIntervalS:    30.0,
		LoopWatchdogProbeTimeoutS:     10.0,
		LoopWatchdogMaxStrikes:        3,
		BotLoopGuard:                  BotLoopGuard{Enabled: true, MaxEvents: 20, WindowSeconds: 300, CooldownSeconds: 600},
		StartupWatchdog:               true,
		StartupWatchdogTimeoutSeconds: 300,
		WriteSessionsJSON:             true,
		MultiplexProfiles:             Bool(true),
		AutoMultiplexMigration:        true,
		ProfileRoutes:                 []ProfileRoute{},
		ScaleToZero:                   ScaleToZero{IdleTimeoutMinutes: 2},
		RestartLoopGuard:              RestartLoopGuard{MaxRestarts: 3, WindowSeconds: 60, MaxGapSeconds: 300},
		RespawnStorm:                  RespawnStorm{MaxStarts: 5, WindowSeconds: 120},
		MaxInboundMediaBytes:          134217728,
		TrustEnv:                      true,
		MediaDeliveryAllowDirs:        ListOf(),
		TrustRecentFiles:              true,
		TrustRecentFilesSeconds:       600,
		APIServer:                     APIServerSettings{MaxConcurrentRuns: 10},
	}
}

// Defaults of the streaming section
func defaultStreaming() StreamingConfig {
	return StreamingConfig{Enabled: Bool(false), Transport: StreamAuto, EditInterval: 0.8, BufferThreshold: 24, Cursor: " ▉"}
}

// Defaults of the sessions section
func defaultSessions() SessionsConfig {
	return SessionsConfig{
		AutoPrune: new(false), RetentionDays: 90, AutoArchiveDays: 3, VacuumAfterPrune: true, MinVacuumIntervalDays: 30, MinIntervalHours: 24,
		FTSOptimizeNotice: NoticeAdvise, CJKFTS: true, SearchSlowMS: 1000, MaxResumeMessages: 20000, MaxExportMessages: 20000,
		OrphanEndGraceS: new(300.0),
	}
}

// Documentation of the gateway, streaming and sessions sections
func gatewayDocs() map[string]string {
	return map[string]string{
		"gateway":                                          "Gateway settings (messaging platforms: Telegram, Discord, Slack, ...).",
		"gateway.unmanaged_idle_exit_seconds":              "Client-auto-started gateway only; 0 disables.",
		"gateway.signal_interrupt_grace_timeout":           "Seconds a SIGTERM-interrupted agent unwinds before teardown (short: service stop budgets).",
		"gateway.service_install_choice":                   "null | install | decline; never authorizes ordinary launch installation.",
		"gateway.delivery_ledger":                          "Durable delivery-obligation ledger: final responses are recorded in state.db around the platform send; a gateway that died between finalize and platform ACK redelivers on next boot (ambiguous cases carry a \"recovered reply, may be a duplicate\" marker; at-least-once). Disable to lose in-flight final responses on crash or restart.",
		"gateway.platform_connect_timeout":                 "Seconds to wait for one platform to connect at startup or reconnect; raise on \"discord connect timed out\" loops (many slash commands to sync). 0 or negative = wait forever. Bridged to ICEBATH_GATEWAY_PLATFORM_CONNECT_TIMEOUT, which wins if set explicitly.",
		"gateway.loop_watchdog":                            "Event-loop liveness watchdog: after consecutive missed probes it dumps all-thread stacks and hard-exits with the service-restart code so systemd or launchd revives the process instead of leaving a wedged-but-alive zombie. Set false to disable.",
		"gateway.loop_watchdog_probe_interval_s":           "Seconds between watchdog probes.",
		"gateway.loop_watchdog_probe_timeout_s":            "Seconds before an unprocessed probe counts as a miss.",
		"gateway.loop_watchdog_max_strikes":                "Consecutive misses before hard-exit 75 (~90-120s of sustained loop block at the defaults).",
		"gateway.allow_all_users":                          "Allow all users without allowlists (security opt-in).",
		"gateway.bot_loop_guard":                           "Bot-to-bot loop guard: admitted bot messages per conversation before a cooldown.",
		"gateway.startup_watchdog":                         "Startup-liveness watchdog armed at process entry that hard-exits 75 if the loop isn't live within the deadline. Armed before config loads, so the gateway bridges these to ICEBATH_STARTUP_WATCHDOG and ICEBATH_STARTUP_WATCHDOG_TIMEOUT_S and re-arms the live handle; explicit env wins.",
		"gateway.startup_watchdog_timeout_seconds":         "Startup watchdog deadline in seconds.",
		"gateway.write_sessions_json":                      "Keep writing the legacy ~/.icebath/sessions/sessions.json mirror of the routing index (primary copy: the state.db gateway_routing table). true for external tooling and downgrade safety; false stops producing the file.",
		"gateway.multiplex_profiles":                       "One gateway for every profile on this host: the DEFAULT profile's gateway also connects each named profile's bots (their own .env and config.yaml, per-profile secret scope) and stamps the profile into session keys. This is the ONLY supported topology: an explicit false still parses but is warned about and IGNORED for process topology, and `icebath gateway migrate --multiplex` folds any per-profile fleet that is left. An UNSET key is a request: at boot the gateway runs the migration preflight and stays standalone (logging why) while a secondary still runs its own gateway or a blocker exists, then converges once that is resolved. On a host that had pinned false, `/p/<profile>/` on the default listener goes from 404 to served, and eager multi-profile activation turns secret scope fail-closed. Two profiles configuring the same bot token cannot be served together; `icebath profile create --clone` leaves messaging channels behind unless --clone-channels is passed.",
		"gateway.auto_multiplex_migration":                 "May `icebath update` fold this install onto a multiplexed default gateway by itself? true (the default) migrates a multi-profile install automatically after an update when nothing blocks it. false chooses WHEN you converge, not whether: the fold is left to you to run by hand. Only the AUTOMATIC path reads this; `icebath gateway migrate --multiplex` is explicit and proceeds.",
		"gateway.profile_routes":                           "Route inbound chats of the default profile's bots to another profile: [{profile, platform, chat_id|user_id|guild_id|...}]. The most-specific match wins; only read by the multiplexing default gateway.",
		"gateway.scale_to_zero":                            "Scale-to-zero idle TIMEOUT only. When an instance is opted in via the Labs toggle (ICEBATH_SCALE_TO_ZERO env stamp) AND messaging is relay-only or absent AND a wakeUrl is registered, the relay transport goes dormant so the platform (e.g. Fly autostop) can suspend the machine; it wakes on the wakeUrl poke. Enablement is the Labs toggle, never a config key. 0 or negative = default.",
		"gateway.restart_loop_guard":                       "Auto-resume restart-loop breaker. A supervisor-revived gateway auto-resumes the SIGTERM-interrupted session; if that turn keeps triggering the kill, boots no more than `max_gap_seconds` apart (floored by `window_seconds`) chain, and after `max_restarts` auto-resume is SKIPPED for that boot (inbound messages still served). max_restarts=0 disables.",
		"gateway.respawn_storm":                            "Respawn-storm circuit breaker: counts (re)starts in a sliding window and sleeps an exponential backoff before booting so a crash-looping supervisor can't hammer the process. max_starts <= 0 disables. Env escape hatches: ICEBATH_GATEWAY_MAX_STARTS and ICEBATH_GATEWAY_START_WINDOW_S.",
		"gateway.message_timestamps":                       "Prefix user messages IN THE MODEL'S CONTEXT with a timestamp (e.g. \"[Tue 2026-04-28 13:40:53 CEST]\") for temporal awareness. Persisted transcripts stay clean (the timestamp is message metadata regardless), so enabling later surfaces past send-times too.",
		"gateway.max_inbound_media_bytes":                  "Max bytes of inbound image, audio or video the gateway buffers into RAM and caches to disk. Media is read fully into memory first, so unbounded uploads or huge remote URLs can OOM-kill constrained deployments. Enforced for every adapter. 0 = no cap. Default 128 MiB.",
		"gateway.trust_env":                                "Let adapters read HTTP_PROXY, HTTPS_PROXY, NO_PROXY and SSL_CERT_FILE from the environment and auto-detect system proxies. false when the gateway inherits a proxy it must not use. Per-platform vars (DISCORD_PROXY, TELEGRAM_PROXY, ...) are still honored.",
		"gateway.strict":                                   "Media delivery. false: any emitted file path is delivered natively unless under the credential and system denylist (/etc, /proc, ~/.ssh, ~/.aws, ~/.icebath/.env, auth.json). true: files must be under the icebath cache, media_delivery_allow_dirs, or fresher than trust_recent_files_seconds, recommended for public-facing gateways so prompt injection can't exfiltrate host secrets. Bridged to ICEBATH_MEDIA_DELIVERY_STRICT.",
		"gateway.media_delivery_allow_dirs":                "Extra roots (project and scratch dirs, mounted shares) from which bare file paths may be uploaded; the icebath cache is always trusted. A list of absolute paths or one path-list-separated string; tildes expanded. Bridged to ICEBATH_MEDIA_ALLOW_DIRS. Honored in both modes.",
		"gateway.trust_recent_files":                       "Trust files whose mtime is within trust_recent_files_seconds even outside the cache and allowlist (e.g. `pandoc -o /tmp/report.pdf`); system paths stay blocked. false = pure-allowlist mode. Bridged to ICEBATH_MEDIA_TRUST_RECENT_FILES. Only consulted when strict is true.",
		"gateway.trust_recent_files_seconds":               "Recency window in seconds; 600 covers a multi-tool turn. Bridged to ICEBATH_MEDIA_TRUST_RECENT_SECONDS. Only consulted when strict is true.",
		"gateway.api_server":                               "OpenAI-compatible API server platform.",
		"gateway.api_server.max_concurrent_runs":           "Max concurrent agent runs. Requests to /v1/chat/completions, /v1/responses, and /v1/runs beyond this get HTTP 429 + Retry-After, bounding CPU, memory and LLM-quota exhaustion from a request flood. 0 = no cap.",
		"gateway.api_server.history_tool_output_max_chars": "Cap (chars) on each tool output and tool-call argument string in the stored /v1/responses conversation history used for previous_response_id chaining. The stored history is cumulative, so a few large tool outputs can make one response store write several hundred KB. 0 = store tool outputs verbatim.",
		"streaming":                           "Real-time token streaming to messaging platforms (gateway; restart after enabling). Off by default: costs extra edit and draft API calls per response.",
		"streaming.enabled":                   "When false, each response is delivered as one final message.",
		"streaming.transport":                 "auto = native drafts where supported (Telegram DMs, Bot API 9.5+), edits elsewhere (Discord, Slack, Matrix, Telegram groups); draft = drafts with edit fallback; edit = progressive editMessageText only; off = disabled.",
		"streaming.edit_interval":             "Minimum seconds between progressive edits (Telegram's ~1 edit/s flood envelope).",
		"streaming.buffer_threshold":          "Flush the buffer once this many chars accumulate, so short replies feel instant.",
		"streaming.cursor":                    "Cursor glyph appended to the in-progress message.",
		"streaming.fresh_final_after_seconds": "Telegram only: when >0, if the preview was visible at least this many seconds the final edit is sent as a fresh message so the timestamp reflects completion.",
		"sessions":                            "Automatic cleanup of ~/.icebath/state.db, which otherwise grows without bound and slows FTS5 inserts, /resume listing, and insights queries.",
		"sessions.auto_prune":                 "Prune ENDED sessions inactive for retention_days (activity = freshest of live activity, latest message, creation) about once per min_interval_hours at CLI, gateway and cron startup. Open, pinned, or mid-turn sessions are never deleted; stale automation sessions whose process died are closed, then get a full retention window before removal. Off by default (deleting history is opt-in). While config.yaml has no auto_prune key, every surface asks once the disk holding state.db runs low; either answer writes the key and ends the question.",
		"sessions.retention_days":             "Inactive days of ended-session history to keep (= `icebath sessions prune`).",
		"sessions.auto_archive":               "Auto-archive (soft-hide, never delete) sessions with no activity for auto_archive_days, once per min_interval_hours. Pinned sessions are exempt.",
		"sessions.auto_archive_days":          "Idle days before auto-archive hides a session (only when auto_archive is true).",
		"sessions.show_subagents":             "List delegate_task subagent runs in session lists (desktop sidebar, dashboard, session.list), nested under their parent. Off by default: they are machinery, not conversations.",
		"sessions.vacuum_after_prune":         "VACUUM after a prune that deleted rows (SQLite never reclaims disk on DELETE). It blocks writes (~seconds per 100MB): only at startup, only when at least 1 session went AND freelist/page_count > 25%.",
		"sessions.min_vacuum_interval_days":   "Minimum days between VACUUM rewrites; pruning keeps its normal cadence.",
		"sessions.min_interval_hours":         "Minimum hours between auto-maintenance runs (tracked in state.db, shared across processes).",
		"sessions.fts_optimize_notice":        "Notice about the compact FTS layout (reclaims ~60%+ of state.db). OPT-IN: legacy indexes stay until `icebath sessions optimize-storage` runs, since the rebuild is disk-heavy on large DBs. advise = `icebath update` prints a one-line notice with the reclaimable size when a legacy index is detected; require = the same notices worded as REQUIRED (tooling may gate on it); off = none.",
		"sessions.cjk_fts":                    "CJK-bigram search index (messages_fts_cjk). When the extension is built (~/.icebath/lib/libfts5_cjk.so), 1-2 char CJK terms get exact index matches instead of LIKE scans. true = use when present (inert otherwise); false = never load or serve it. Bridged to ICEBATH_CJK_FTS.",
		"sessions.search_slow_ms":             "Slow session-search threshold (ms): searches at or above it log one INFO line with the routing path (fts_cjk, fts5, trigram, like_scan). 0 logs every search. Bridged to ICEBATH_SEARCH_SLOW_MS.",
		"sessions.max_resume_messages":        "Transcript guard (a runaway 100k+ row session can exhaust memory when materialized at once; 0 disables): max active messages (across the compression lineage) for interactive resume.",
		"sessions.max_export_messages":        "Max active messages per session for in-memory export (`icebath sessions export`); checked per session, so full-DB backups of small sessions work.",
		"sessions.orphan_end_grace_s":         "Seconds before a local session with no viewer is ended (resumable) once its turn settles; 0 = never; after a restart also >= 6h idle. While unset, dashboard.ws_orphan_reap_grace_s 0 or startup_orphan_sweep false still opt out.",
	}
}
