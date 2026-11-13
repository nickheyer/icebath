package config

import (
	"reflect"

	"gopkg.in/yaml.v3"
)

// One messaging platform block under platforms, gateway.platforms, or a root platform section, adapter keys kept by name
type PlatformBlock struct {
	Enabled                    Scalar               `yaml:"enabled,omitempty"`
	Token                      *string              `yaml:"token,omitempty"`
	APIKey                     *string              `yaml:"api_key,omitempty"`
	HomeChannel                *HomeChannel         `yaml:"home_channel,omitempty"`
	ReplyToMode                ReplyToMode          `yaml:"reply_to_mode,omitempty"`
	GatewayRestartNotification Scalar               `yaml:"gateway_restart_notification,omitempty"`
	TypingIndicator            Scalar               `yaml:"typing_indicator,omitempty"`
	TypingStatusText           *string              `yaml:"typing_status_text,omitempty"`
	ChannelOverrides           Map[ChannelOverride] `yaml:"channel_overrides,omitempty"`
	Extra                      Map[Raw]             `yaml:"extra,omitempty"`
	PlatformShared             `yaml:",inline"`
	rest                       rest
}

// Keys every platform block shares, bridged into the adapter settings by the gateway
type PlatformShared struct {
	UnauthorizedDMBehavior          string      `yaml:"unauthorized_dm_behavior,omitempty"`
	NoticeDelivery                  string      `yaml:"notice_delivery,omitempty"`
	ReplyPrefix                     *string     `yaml:"reply_prefix,omitempty"`
	ReplyInThread                   Scalar      `yaml:"reply_in_thread,omitempty"`
	CronContinuableSurface          Scalar      `yaml:"cron_continuable_surface,omitempty"`
	RequireMention                  Scalar      `yaml:"require_mention,omitempty"`
	SendReadReceipts                Scalar      `yaml:"send_read_receipts,omitempty"`
	AllowedChats                    StringList  `yaml:"allowed_chats,omitempty"`
	GroupAllowedChats               StringList  `yaml:"group_allowed_chats,omitempty"`
	AllowedTopics                   StringList  `yaml:"allowed_topics,omitempty"`
	FreeResponseChannels            StringList  `yaml:"free_response_channels,omitempty"`
	MentionPatterns                 StringList  `yaml:"mention_patterns,omitempty"`
	ExclusiveBotMentions            Scalar      `yaml:"exclusive_bot_mentions,omitempty"`
	ObserveUnmentionedGroupMessages Scalar      `yaml:"observe_unmentioned_group_messages,omitempty"`
	DMPolicy                        string      `yaml:"dm_policy,omitempty"`
	AllowFrom                       StringList  `yaml:"allow_from,omitempty"`
	AllowAdminFrom                  StringList  `yaml:"allow_admin_from,omitempty"`
	UserAllowedCommands             StringList  `yaml:"user_allowed_commands,omitempty"`
	GroupPolicy                     string      `yaml:"group_policy,omitempty"`
	GroupAllowFrom                  StringList  `yaml:"group_allow_from,omitempty"`
	GroupAllowAdminFrom             StringList  `yaml:"group_allow_admin_from,omitempty"`
	GroupUserAllowedCommands        StringList  `yaml:"group_user_allowed_commands,omitempty"`
	ChannelSkillBindings            Raw         `yaml:"channel_skill_bindings,omitempty"`
	ChannelPrompts                  Map[string] `yaml:"channel_prompts,omitempty"`
}

type platformBlockFields PlatformBlock

// Decodes the mapping and keeps adapter keys
func (p *PlatformBlock) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*platformBlockFields)(p), &p.rest)
}

// Writes the mapping with adapter keys after the named ones
func (p PlatformBlock) MarshalYAML() (any, error) {
	return encodeMapping(platformBlockFields(p), p.rest)
}

// Adapter key written directly in the block, the form the gateway promotes into the adapter settings
func (p PlatformBlock) Setting(key string) *yaml.Node {
	return p.rest.copyOf(key)
}

// Adapter keys written directly in the block, in file order
func (p PlatformBlock) Settings() []string {
	return p.rest.names()
}

// Whether replies thread to the triggering message
type ReplyToMode string

// Reply threading modes
const (
	ReplyOff   ReplyToMode = "off"
	ReplyFirst ReplyToMode = "first"
	ReplyAll   ReplyToMode = "all"
)

// Accepted reply threading modes, a false value reading as off
func ReplyToModes() []ReplyToMode {
	return []ReplyToMode{ReplyOff, ReplyFirst, ReplyAll}
}

// Unauthorized direct message behaviors
func UnauthorizedDMBehaviors() []string {
	return []string{"pair", "ignore", "decline"}
}

// Notice delivery modes
func NoticeDeliveries() []string {
	return []string{"public", "private"}
}

// Default delivery target of a platform
type HomeChannel struct {
	Platform string `yaml:"platform"`
	ChatID   string `yaml:"chat_id"`
	Name     string `yaml:"name,omitempty"`
	ThreadID string `yaml:"thread_id,omitempty"`
	UserID   string `yaml:"user_id,omitempty"`
	ScopeID  string `yaml:"scope_id,omitempty"`
	rest     rest
}

type homeChannelFields HomeChannel

// Decodes the mapping and keeps unknown keys
func (h *HomeChannel) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*homeChannelFields)(h), &h.rest)
}

// Writes the mapping with unknown keys after the named ones
func (h HomeChannel) MarshalYAML() (any, error) {
	return encodeMapping(homeChannelFields(h), h.rest)
}

// Per-channel reply policy, model, provider and prompt override
type ChannelOverride struct {
	Model        *string `yaml:"model,omitempty"`
	Provider     *string `yaml:"provider,omitempty"`
	SystemPrompt *string `yaml:"system_prompt,omitempty"`
	AllowSilence *bool   `yaml:"allow_silence,omitempty"`
	rest         rest
}

type channelOverrideFields ChannelOverride

// Decodes the mapping and keeps unknown keys
func (c *ChannelOverride) UnmarshalYAML(n *yaml.Node) error {
	return decodeEntry(n, (*channelOverrideFields)(c), &c.rest)
}

// Writes the mapping with unknown keys after the named ones
func (c ChannelOverride) MarshalYAML() (any, error) {
	return encodeMapping(channelOverrideFields(c), c.rest)
}

// Slack section
type SlackSection struct {
	RequireMention          bool        `yaml:"require_mention"`
	FreeResponseChannels    StringList  `yaml:"free_response_channels"`
	AllowedChannels         StringList  `yaml:"allowed_channels"`
	RequireMentionChannels  StringList  `yaml:"require_mention_channels"`
	IgnoreOtherUserMentions bool        `yaml:"ignore_other_user_mentions"`
	ThreadRequireMention    bool        `yaml:"thread_require_mention"`
	ChannelPrompts          Map[string] `yaml:"channel_prompts"`
	rest                    rest
}

type slackFields SlackSection

// Decodes the mapping and keeps adapter keys
func (s *SlackSection) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*slackFields)(s), &s.rest)
}

// Writes the mapping with adapter keys after the named ones
func (s SlackSection) MarshalYAML() (any, error) {
	return encodeMapping(slackFields(s), s.rest)
}

// Discord section
type DiscordSection struct {
	RequireMention                       bool                  `yaml:"require_mention"`
	FreeResponseChannels                 StringList            `yaml:"free_response_channels"`
	AllowedChannels                      StringList            `yaml:"allowed_channels"`
	AutoThread                           bool                  `yaml:"auto_thread"`
	FreeResponseAutoThread               bool                  `yaml:"free_response_auto_thread"`
	ThreadRequireMention                 bool                  `yaml:"thread_require_mention"`
	BotsRequireInlineMention             bool                  `yaml:"bots_require_inline_mention"`
	HistoryBackfill                      bool                  `yaml:"history_backfill"`
	HistoryBackfillLimit                 int                   `yaml:"history_backfill_limit"`
	MissedMessageBackfill                MissedMessageBackfill `yaml:"missed_message_backfill"`
	Reactions                            bool                  `yaml:"reactions"`
	WebsocketLivenessIntervalSeconds     float64               `yaml:"websocket_liveness_interval_seconds"`
	WebsocketLivenessFailureThreshold    int                   `yaml:"websocket_liveness_failure_threshold"`
	WebsocketHeartbeatAckMaxAgeSeconds   float64               `yaml:"websocket_heartbeat_ack_max_age_seconds"`
	WebsocketMaxLatencySeconds           float64               `yaml:"websocket_max_latency_seconds"`
	WebsocketEventMaxSilenceSeconds      float64               `yaml:"websocket_event_max_silence_seconds"`
	ChannelPrompts                       Map[string]           `yaml:"channel_prompts"`
	DMRoleAuthGuild                      Scalar                `yaml:"dm_role_auth_guild"`
	ServerActions                        StringList            `yaml:"server_actions"`
	AllowAnyAttachment                   bool                  `yaml:"allow_any_attachment"`
	MaxAttachmentBytes                   int64                 `yaml:"max_attachment_bytes"`
	ApprovalMentions                     bool                  `yaml:"approval_mentions"`
	VoiceChannelInactivityTimeoutSeconds int                   `yaml:"voice_channel_inactivity_timeout_seconds"`
	VoicePlaybackTimeoutSeconds          int                   `yaml:"voice_playback_timeout_seconds"`
	VoiceFX                              VoiceFX               `yaml:"voice_fx"`
	rest                                 rest
}

type discordFields DiscordSection

// Decodes the mapping and keeps adapter keys
func (d *DiscordSection) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*discordFields)(d), &d.rest)
}

// Writes the mapping with adapter keys after the named ones
func (d DiscordSection) MarshalYAML() (any, error) {
	return encodeMapping(discordFields(d), d.rest)
}

// Discord server tool actions, an empty setting allowing all
func DiscordServerActions() []string {
	return []string{
		"list_guilds", "server_info", "list_channels", "channel_info", "list_roles", "member_info", "search_members",
		"fetch_messages", "list_pins", "pin_message", "unpin_message", "create_thread", "add_role", "remove_role",
	}
}

// Replay of messages missed while offline
type MissedMessageBackfill struct {
	Enabled       bool       `yaml:"enabled"`
	Channels      StringList `yaml:"channels"`
	WindowSeconds float64    `yaml:"window_seconds"`
	Limit         int        `yaml:"limit"`
	MaxDispatches int        `yaml:"max_dispatches"`
	MaxAttempts   int        `yaml:"max_attempts"`
	rest          rest
}

type missedMessageBackfillFields MissedMessageBackfill

// Decodes the mapping and keeps unknown keys
func (m *MissedMessageBackfill) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*missedMessageBackfillFields)(m), &m.rest)
}

// Writes the mapping with unknown keys after the named ones
func (m MissedMessageBackfill) MarshalYAML() (any, error) {
	return encodeMapping(missedMessageBackfillFields(m), m.rest)
}

// Discord voice channel software mixer
type VoiceFX struct {
	Enabled        bool     `yaml:"enabled"`
	AmbientEnabled bool     `yaml:"ambient_enabled"`
	AmbientPath    string   `yaml:"ambient_path"`
	AmbientGain    float64  `yaml:"ambient_gain"`
	DuckGain       float64  `yaml:"duck_gain"`
	SpeechGain     float64  `yaml:"speech_gain"`
	AckEnabled     bool     `yaml:"ack_enabled"`
	AckPhrases     []string `yaml:"ack_phrases"`
	LeadSilenceMS  *int     `yaml:"lead_silence_ms,omitempty"`
	rest           rest
}

type voiceFXFields VoiceFX

// Decodes the mapping and keeps unknown keys
func (v *VoiceFX) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*voiceFXFields)(v), &v.rest)
}

// Writes the mapping with unknown keys after the named ones
func (v VoiceFX) MarshalYAML() (any, error) {
	return encodeMapping(voiceFXFields(v), v.rest)
}

// WhatsApp section
type WhatsAppSection struct {
	ReplyPrefix *string `yaml:"reply_prefix,omitempty"`
	rest        rest
}

type whatsAppFields WhatsAppSection

// Decodes the mapping and keeps adapter keys
func (w *WhatsAppSection) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*whatsAppFields)(w), &w.rest)
}

// Writes the mapping with adapter keys after the named ones
func (w WhatsAppSection) MarshalYAML() (any, error) {
	return encodeMapping(whatsAppFields(w), w.rest)
}

// Telegram section
type TelegramSection struct {
	Reactions      bool          `yaml:"reactions"`
	ChannelPrompts Map[string]   `yaml:"channel_prompts"`
	AllowedChats   StringList    `yaml:"allowed_chats"`
	Extra          TelegramExtra `yaml:"extra"`
	rest           rest
}

type telegramFields TelegramSection

// Decodes the mapping and keeps adapter keys
func (t *TelegramSection) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*telegramFields)(t), &t.rest)
}

// Writes the mapping with adapter keys after the named ones
func (t TelegramSection) MarshalYAML() (any, error) {
	return encodeMapping(telegramFields(t), t.rest)
}

// Telegram rich message and topic settings
type TelegramExtra struct {
	RichMessages         bool `yaml:"rich_messages"`
	RichDrafts           bool `yaml:"rich_drafts"`
	AllowCJKRichMessages bool `yaml:"allow_cjk_rich_messages"`
	DMTopicAutoTransfer  bool `yaml:"dm_topic_auto_transfer"`
	rest                 rest
}

type telegramExtraFields TelegramExtra

// Decodes the mapping and keeps adapter keys
func (t *TelegramExtra) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*telegramExtraFields)(t), &t.rest)
}

// Writes the mapping with adapter keys after the named ones
func (t TelegramExtra) MarshalYAML() (any, error) {
	return encodeMapping(telegramExtraFields(t), t.rest)
}

// Mattermost section
type MattermostSection struct {
	RequireMention       bool        `yaml:"require_mention"`
	FreeResponseChannels StringList  `yaml:"free_response_channels"`
	AllowedChannels      StringList  `yaml:"allowed_channels"`
	ChannelPrompts       Map[string] `yaml:"channel_prompts"`
	rest                 rest
}

type mattermostFields MattermostSection

// Decodes the mapping and keeps adapter keys
func (m *MattermostSection) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*mattermostFields)(m), &m.rest)
}

// Writes the mapping with adapter keys after the named ones
func (m MattermostSection) MarshalYAML() (any, error) {
	return encodeMapping(mattermostFields(m), m.rest)
}

// Matrix section
type MatrixSection struct {
	RequireMention    bool       `yaml:"require_mention"`
	FreeResponseRooms StringList `yaml:"free_response_rooms"`
	AllowedRooms      StringList `yaml:"allowed_rooms"`
	rest              rest
}

type matrixFields MatrixSection

// Decodes the mapping and keeps adapter keys
func (m *MatrixSection) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*matrixFields)(m), &m.rest)
}

// Writes the mapping with adapter keys after the named ones
func (m MatrixSection) MarshalYAML() (any, error) {
	return encodeMapping(matrixFields(m), m.rest)
}

// Generic block view of any value, for reading a root platform section through the shared platform keys
func blockView(v any) (PlatformBlock, error) {
	n, err := encodeValue(reflect.ValueOf(v))
	if err != nil {
		return PlatformBlock{}, err
	}
	var b PlatformBlock
	err = b.UnmarshalYAML(n)
	return b, err
}

// Defaults of the root platform sections
func defaultSlack() SlackSection {
	return SlackSection{RequireMention: true}
}

// Defaults of the Discord section
func defaultDiscord() DiscordSection {
	return DiscordSection{
		RequireMention:                       true,
		AutoThread:                           true,
		BotsRequireInlineMention:             true,
		HistoryBackfill:                      true,
		HistoryBackfillLimit:                 50,
		MissedMessageBackfill:                MissedMessageBackfill{WindowSeconds: 21600, Limit: 100, MaxDispatches: 10, MaxAttempts: 3},
		Reactions:                            true,
		WebsocketLivenessIntervalSeconds:     15,
		WebsocketLivenessFailureThreshold:    2,
		WebsocketHeartbeatAckMaxAgeSeconds:   60,
		WebsocketMaxLatencySeconds:           30,
		WebsocketEventMaxSilenceSeconds:      14400,
		DMRoleAuthGuild:                      Text(""),
		MaxAttachmentBytes:                   33554432,
		VoiceChannelInactivityTimeoutSeconds: 300,
		VoicePlaybackTimeoutSeconds:          120,
		VoiceFX: VoiceFX{
			AmbientEnabled: true, AmbientGain: 0.18, DuckGain: 0.06, SpeechGain: 1.0, AckEnabled: true,
			AckPhrases: []string{"Let me look into that.", "One moment.", "Checking on that now.", "Give me a sec.", "On it."},
		},
	}
}

// Documentation of the platform sections
func platformDocs() map[string]string {
	return map[string]string{
		"slack.require_mention":                            "Require an @mention to respond in channels.",
		"slack.free_response_channels":                     "Comma-separated channel IDs answered without mention.",
		"slack.allowed_channels":                           "If set, ONLY respond in these channel IDs (whitelist).",
		"slack.require_mention_channels":                   "Channel IDs where an @mention is ALWAYS required.",
		"slack.ignore_other_user_mentions":                 "Ignore messages whose first token @mentions another user unless the bot is also mentioned. Env: SLACK_IGNORE_OTHER_USER_MENTIONS.",
		"slack.thread_require_mention":                     "Require an @mention in thread replies too.",
		"slack.channel_prompts":                            "Per-channel ephemeral system prompts.",
		"discord.require_mention":                          "Require an @mention to respond in server channels.",
		"discord.free_response_channels":                   "Comma-separated channel IDs answered without mention.",
		"discord.allowed_channels":                         "If set, ONLY respond in these channel IDs (whitelist).",
		"discord.auto_thread":                              "Auto-create threads on @mention in channels (like Slack).",
		"discord.free_response_auto_thread":                "Free-response channels reply inline by default; true also gives each top-level message in them its own thread (still mention-free). Env: DISCORD_FREE_RESPONSE_AUTO_THREAD.",
		"discord.thread_require_mention":                   "Require an @mention in threads too (multi-bot threads).",
		"discord.bots_require_inline_mention":              "Bot authors must type @thisbot to trigger a reply; Discord reply pings alone do not count. Set false only for trusted legacy relays. Humans are unaffected.",
		"discord.history_backfill":                         "Prepend recent channel scrollback when triggered (recovers messages gated out by require_mention); history_backfill_limit = max messages scanned.",
		"discord.missed_message_backfill":                  "Replay messages missed while offline, after reconnect or startup.",
		"discord.missed_message_backfill.channels":         "Comma-separated channel IDs; empty uses free_response_channels.",
		"discord.missed_message_backfill.window_seconds":   "Only inspect messages from the last 6 hours.",
		"discord.missed_message_backfill.limit":            "Global cap on messages scanned per reconnect.",
		"discord.missed_message_backfill.max_dispatches":   "Cap on recovered messages dispatched per reconnect.",
		"discord.missed_message_backfill.max_attempts":     "Lifetime re-dispatch cap for one message, whatever its outcome.",
		"discord.reactions":                                "Add reactions to messages during processing.",
		"discord.websocket_liveness_interval_seconds":      "Gateway transport health probe: inspects the WebSocket's ready, open and heartbeat state (never REST) as proof events still arrive. Any value 0 disables it.",
		"discord.websocket_event_max_silence_seconds":      "Dispatch-side dimension: a socket that ACKs heartbeats but delivers no events for this long is treated as deaf. 4 h absorbs a quiet server overnight; 0 disables it.",
		"discord.channel_prompts":                          "Per-channel ephemeral system prompts (forum parents apply to child threads).",
		"discord.dm_role_auth_guild":                       "Opt-in DM role auth: DISCORD_ALLOWED_ROLES normally authorizes guild messages only (DMs need DISCORD_ALLOWED_USERS). A guild ID here also authorizes DMs from that guild's members holding the allowed role. Unset, \"\" or 0 = off.",
		"discord.server_actions":                           "discord and discord_admin tools: allowed actions (comma string or YAML list; empty = all, subject to bot intents; unknown names dropped with a warning): list_guilds, server_info, list_channels, channel_info, list_roles, member_info, search_members, fetch_messages, list_pins, pin_message, unpin_message, create_thread, add_role, remove_role.",
		"discord.allow_any_attachment":                     "DEPRECATED no-op (uploads are always cached; messaging auth is the gate), kept so existing configs don't error. Env: DISCORD_ALLOW_ANY_ATTACHMENT.",
		"discord.max_attachment_bytes":                     "Max bytes per cached attachment (held in memory while written); 0 = no cap. Env: DISCORD_MAX_ATTACHMENT_BYTES.",
		"discord.approval_mentions":                        "Mention allowed users on approval prompts so owners notice them in shared channels. Env: DISCORD_APPROVAL_MENTIONS.",
		"discord.voice_channel_inactivity_timeout_seconds": "Voice-channel inactivity timeout (seconds); 0 = stay until `/voice leave`.",
		"discord.voice_playback_timeout_seconds":           "Minimum seconds before force-stopping a voice channel playback; the adapter probes clip duration and extends this floor so long TTS isn't cut off.",
		"discord.voice_fx":                                 "Voice-channel software mixer: ambient \"thinking\" bed, verbal acks and TTS OVERLAP (ambient ducked) vs stop-and-swap.",
		"discord.voice_fx.enabled":                         "Master switch for the mixer subsystem.",
		"discord.voice_fx.ambient_enabled":                 "Play the idle \"thinking\" bed while tools run.",
		"discord.voice_fx.ambient_path":                    "Custom loop audio file; \"\" = synthesised pad.",
		"discord.voice_fx.ambient_gain":                    "Idle bed loudness, 0.0-1.0.",
		"discord.voice_fx.duck_gain":                       "Ambient loudness while speech plays.",
		"discord.voice_fx.speech_gain":                     "TTS and ack loudness, 0.0-1.0.",
		"discord.voice_fx.ack_enabled":                     "Speak a short phrase before the first tool call.",
		"discord.voice_fx.ack_phrases":                     "Picked at random; [] disables phrases.",
		"discord.voice_fx.lead_silence_ms":                 "Silence (ms) prepended to each clip so the voice socket warm-up does not clip the first word (default 200).",
		"whatsapp.reply_prefix":                            "null = the built-in \"icebath\" header; \"\" disables; \\n allowed.",
		"telegram.reactions":                               "Add reactions to messages during processing.",
		"telegram.channel_prompts":                         "Per-chat and per-topic ephemeral system prompts (topics inherit from the parent group).",
		"telegram.allowed_chats":                           "If set, ONLY respond in these group and supergroup chat IDs.",
		"telegram.extra.rich_messages":                     "Bot API 10.1 native rich messages (tables, task lists, math). Off = legacy MarkdownV2, since rich messages are hard to copy as plain text.",
		"telegram.extra.rich_drafts":                       "Experimental rich DM stream previews; off because Telegram Desktop and macOS overlay draft frames.",
		"telegram.extra.allow_cjk_rich_messages":           "CJK stays on legacy MarkdownV2 (Telegram Desktop and macOS garble rich CJK); set true on an unaffected client to get native rich tables for CJK.",
		"telegram.extra.dm_topic_auto_transfer":            "/topic mode: a root-DM message opens a fresh topic.",
		"mattermost.require_mention":                       "Require an @mention to respond in channels.",
		"mattermost.free_response_channels":                "Comma-separated channel IDs answered without mention.",
		"mattermost.allowed_channels":                      "If set, ONLY respond in these channel IDs (whitelist).",
		"mattermost.channel_prompts":                       "Per-channel ephemeral system prompts.",
		"matrix.require_mention":                           "Require an @mention to respond in rooms.",
		"matrix.free_response_rooms":                       "Comma-separated room IDs answered without mention.",
		"matrix.allowed_rooms":                             "If set, ONLY respond in these room IDs (whitelist).",
		"platforms":                                        "Messaging platform blocks keyed by platform name: enabled, token, api_key, home_channel, reply_to_mode, channel_overrides, extra, and adapter keys written directly in the block.",
	}
}
