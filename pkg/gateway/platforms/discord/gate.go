package discord

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Authorization and gate env names snapshotted per adapter at connect time
var gateEnvKeys = []string{
	"DISCORD_ALLOWED_USERS", "DISCORD_ALLOWED_ROLES", "DISCORD_ALLOWED_CHANNELS",
	"DISCORD_IGNORED_CHANNELS", "DISCORD_NO_THREAD_CHANNELS", "DISCORD_FREE_RESPONSE_CHANNELS",
	"DISCORD_MISSED_MESSAGE_BACKFILL_CHANNELS", "DISCORD_ALLOW_ALL_USERS", "DISCORD_ALLOW_BOTS",
	"GATEWAY_ALLOW_ALL_USERS", "GATEWAY_ALLOWED_USERS",
}

var rawUserMentionRE = regexp.MustCompile(`<@!?(\d+)>`)

// Set of strings in the Python set sense
type strSet map[string]struct{}

func newStrSet(items ...string) strSet {
	s := strSet{}
	for _, it := range items {
		s[it] = struct{}{}
	}
	return s
}

func (s strSet) has(k string) bool {
	_, ok := s[k]
	return ok
}

// Whether the two sets share any member
func (s strSet) intersects(o strSet) bool {
	for k := range s {
		if o.has(k) {
			return true
		}
	}
	return false
}

// Boolean from scoped env then config extra then the default, parsed by the flag's truthy shape
func (a *DiscordAdapter) extraOrEnvFlag(ctx context.Context, key, envKey, envDefault string, truthy bool) bool {
	configured, err := gateway.ExtraOrSecret(ctx, a.Config.Extra, key, envKey, nil, false)
	if err != nil || configured == nil {
		configured = envDefault
	}
	if b, ok := configured.(bool); ok {
		return b
	}
	text := strings.ToLower(pystr.Strip(pyrepr.Str(configured)))
	if truthy {
		return text == "true" || text == "1" || text == "yes" || text == "on"
	}
	return !(text == "false" || text == "0" || text == "no" || text == "off")
}

// Whether Discord channel messages require a bot mention
func (a *DiscordAdapter) discordRequireMention(ctx context.Context) bool {
	return a.extraOrEnvFlag(ctx, "require_mention", "DISCORD_REQUIRE_MENTION", "true", false)
}

// Whether free response channels also auto thread, off by default
func (a *DiscordAdapter) discordFreeResponseAutoThread(ctx context.Context) bool {
	return a.extraOrEnvFlag(ctx, "free_response_auto_thread", "DISCORD_FREE_RESPONSE_AUTO_THREAD", "false", true)
}

// Per attachment byte cap, zero unlimited, 32 MiB by default or on an invalid value
func (a *DiscordAdapter) discordMaxAttachmentBytes(ctx context.Context) int64 {
	const def = 32 * 1024 * 1024
	configured, present := a.Config.Extra["max_attachment_bytes"]
	if !present || configured == nil {
		if env := gateway.PlatformGateEnv(ctx, "DISCORD_MAX_ATTACHMENT_BYTES", ""); env != "" {
			configured = env
		} else {
			configured = nil
		}
	}
	if configured == nil || configured == "" {
		return def
	}
	n, err := pyint.Of(configured)
	if err != nil {
		slog.Warn(fmt.Sprintf("[Discord] Invalid max_attachment_bytes value %s, falling back to 32 MiB", pyrepr.Repr(configured)))
		return def
	}
	if n.Sign() <= 0 {
		return 0
	}
	if !n.IsInt64() {
		return math.MaxInt64
	}
	return n.Int64()
}

// Whether a Discord audio attachment is a native voice note
func isDiscordVoiceMessageAttachment(att *discordgo.MessageAttachment) bool {
	return att != nil && att.Waveform != ""
}

// Snapshots every gate env value inside the owning profile's scope
func (a *DiscordAdapter) snapshotGateEnv(ctx context.Context) {
	snap := make(map[string]string, len(gateEnvKeys))
	for _, key := range gateEnvKeys {
		snap[key] = gateway.PlatformGateEnv(ctx, key, "")
	}
	a.mu.Lock()
	a.gateEnvSnapshot = snap
	a.mu.Unlock()
}

// Gate env value from this adapter's snapshot, the scoped env when not snapshotted
func (a *DiscordAdapter) gateEnv(ctx context.Context, name, def string) string {
	a.mu.Lock()
	snap := a.gateEnvSnapshot
	a.mu.Unlock()
	if snap != nil {
		if v, ok := snap[name]; ok {
			if v == "" {
				return def
			}
			return v
		}
	}
	return gateway.PlatformGateEnv(ctx, name, def)
}

// One gate value, the env or snapshot first then the extra of the given or own config
func (a *DiscordAdapter) gateRaw(ctx context.Context, extraKey, envKey string, cfg *gateway.PlatformConfig) any {
	var val string
	if cfg == nil {
		val = a.gateEnv(ctx, envKey, "")
	} else {
		val = gateway.PlatformGateEnv(ctx, envKey, "")
	}
	if val != "" {
		return val
	}
	if cfg == nil {
		cfg = a.Config
	}
	if cfg != nil && cfg.Extra != nil {
		return cfg.Extra[extraKey]
	}
	return nil
}

// Comma separated or list gate value as a set of stripped non empty strings
func gateCSVSet(raw any) strSet {
	out := strSet{}
	if raw == nil {
		return out
	}
	raw = gateway.DecodeJSONListLiteral(raw)
	if items, ok := raw.([]any); ok {
		for _, part := range items {
			if s := pystr.Strip(pyrepr.Str(part)); s != "" {
				out[s] = struct{}{}
			}
		}
		return out
	}
	if items, ok := raw.([]string); ok {
		for _, part := range items {
			if s := pystr.Strip(part); s != "" {
				out[s] = struct{}{}
			}
		}
		return out
	}
	for _, part := range strings.Split(pyrepr.Str(raw), ",") {
		if s := pystr.Strip(part); s != "" {
			out[s] = struct{}{}
		}
	}
	return out
}

// This adapter's DISCORD_ALLOWED_CHANNELS gate
func (a *DiscordAdapter) getAllowedChannels(ctx context.Context) strSet {
	return gateCSVSet(a.gateRaw(ctx, "allowed_channels", "DISCORD_ALLOWED_CHANNELS", nil))
}

// This adapter's DISCORD_IGNORED_CHANNELS gate
func (a *DiscordAdapter) getIgnoredChannels(ctx context.Context) strSet {
	return gateCSVSet(a.gateRaw(ctx, "ignored_channels", "DISCORD_IGNORED_CHANNELS", nil))
}

// This adapter's DISCORD_NO_THREAD_CHANNELS list
func (a *DiscordAdapter) getNoThreadChannels(ctx context.Context) strSet {
	return gateCSVSet(a.gateRaw(ctx, "no_thread_channels", "DISCORD_NO_THREAD_CHANNELS", nil))
}

// This adapter's cleaned DISCORD_ALLOWED_USERS entries with the allowed_users extra as fallback
func (a *DiscordAdapter) getAllowedUsers(ctx context.Context) strSet {
	raw := a.gateRaw(ctx, "allow_from", "DISCORD_ALLOWED_USERS", nil)
	if raw == nil && a.Config != nil && a.Config.Extra != nil {
		raw = a.Config.Extra["allowed_users"]
	}
	out := strSet{}
	for entry := range gateCSVSet(raw) {
		if cleaned := cleanDiscordID(entry); cleaned != "" {
			out[cleaned] = struct{}{}
		}
	}
	return out
}

// This adapter's DISCORD_ALLOWED_ROLES ids in canonical decimal form
func (a *DiscordAdapter) getAllowedRoles(ctx context.Context) (strSet, error) {
	out := strSet{}
	for entry := range gateCSVSet(a.gateRaw(ctx, "allowed_roles", "DISCORD_ALLOWED_ROLES", nil)) {
		entry = pystr.Strip(entry)
		if !isDigitString(entry) {
			continue
		}
		n, ok := pyint.Parse(entry)
		if !ok {
			return nil, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(entry))
		}
		out[n.String()] = struct{}{}
	}
	return out, nil
}

// Numeric ids resolved from username allowlist entries at connect time
func (a *DiscordAdapter) ResolvedAllowlistUserIDs() map[string]struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]struct{}, len(a.usernameResolvedIDs))
	for k := range a.usernameResolvedIDs {
		out[k] = struct{}{}
	}
	return out
}

// Per profile DISCORD_ALLOW_ALL_USERS flag
func (a *DiscordAdapter) discordAllowAllUsers(ctx context.Context) bool {
	raw := a.gateRaw(ctx, "allow_all_users", "DISCORD_ALLOW_ALL_USERS", nil)
	text := ""
	if pyrepr.Truthy(raw) {
		text = pyrepr.Str(raw)
	}
	switch strings.ToLower(pystr.Strip(text)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// Per profile GATEWAY_ALLOW_ALL_USERS flag
func (a *DiscordAdapter) gatewayAllowAllUsers(ctx context.Context) bool {
	switch strings.ToLower(pystr.Strip(a.gateEnv(ctx, "GATEWAY_ALLOW_ALL_USERS", ""))) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// Per profile DISCORD_ALLOW_BOTS mode of none mentions or all
func (a *DiscordAdapter) getAllowBots(ctx context.Context) string {
	raw := a.gateRaw(ctx, "allow_bots", "DISCORD_ALLOW_BOTS", nil)
	text := "none"
	if pyrepr.Truthy(raw) {
		text = pyrepr.Str(raw)
	}
	if mode := pystr.Strip(strings.ToLower(text)); mode != "" {
		return mode
	}
	return "none"
}

// Channel and author pair keying a bot tag continuation window
func botTagDebounceKey(m *inboundMessage) string {
	authorID := ""
	if m.Author != nil {
		authorID = m.Author.ID
	}
	return m.ChannelID + ":" + authorID
}

// Seconds a bot tag keeps its continuation window open
func (a *DiscordAdapter) botTagWindowSeconds() float64 {
	return max(a.textBatchDelaySeconds, a.textBatchSplitDelaySeconds)
}

// Monotonic clock reading in seconds
var monotonic = func() float64 { return float64(time.Since(processStart)) / float64(time.Second) }

var processStart = time.Now()

// Opens a short continuation window after a bot authored tag of this bot
func (a *DiscordAdapter) recordBotTagDebounce(m *inboundMessage) {
	if a.textBatchDelaySeconds <= 0 || m.Author == nil || !m.Author.Bot || !a.selfIsExplicitlyMentioned(m) {
		return
	}
	a.mu.Lock()
	a.botTagDebounceUntil[botTagDebounceKey(m)] = monotonic() + a.botTagWindowSeconds()
	a.mu.Unlock()
}

// Whether an unmentioned bot chunk continues a recent tag, rearming the window on a hit
func (a *DiscordAdapter) isBotTagDebounceContinuation(m *inboundMessage) bool {
	if a.textBatchDelaySeconds <= 0 || m.Author == nil || !m.Author.Bot {
		return false
	}
	key := botTagDebounceKey(m)
	now := monotonic()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.botTagDebounceUntil[key] <= now {
		delete(a.botTagDebounceUntil, key)
		return false
	}
	a.botTagDebounceUntil[key] = now + a.botTagWindowSeconds()
	return true
}

// Channel ids or names needing no mention, a lone star kept for the wildcard check
func (a *DiscordAdapter) discordFreeResponseChannels(ctx context.Context) strSet {
	raw, present := a.Config.Extra["free_response_channels"]
	if !present || raw == nil {
		raw = a.gateEnv(ctx, "DISCORD_FREE_RESPONSE_CHANNELS", "")
	}
	return gateCSVSet(raw)
}

// User mention ids typed in the raw content, plain and legacy nickname forms
func rawMentionedUserIDs(m *inboundMessage) strSet {
	out := strSet{}
	for _, match := range rawUserMentionRE.FindAllStringSubmatch(m.Content, -1) {
		out[match[1]] = struct{}{}
	}
	return out
}

// Bot user of the live client, nil when not connected
func (a *DiscordAdapter) selfUser() *discordgo.User {
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil || client.State == nil {
		return nil
	}
	return client.State.User
}

// Whether the bot is in the mention list or raw mentioned in the content
func (a *DiscordAdapter) selfIsExplicitlyMentioned(m *inboundMessage) bool {
	me := a.selfUser()
	if me == nil {
		return false
	}
	for _, u := range m.Mentions {
		if u != nil && u.ID == me.ID {
			return true
		}
	}
	return rawMentionedUserIDs(m).has(me.ID)
}

// Whether the content carries a literal mention token of this bot
func (a *DiscordAdapter) selfIsRawMentioned(m *inboundMessage) bool {
	me := a.selfUser()
	if me == nil {
		return false
	}
	return rawMentionedUserIDs(m).has(me.ID)
}

// Whether another bot must type an inline mention to trigger this bot, on by default
func (a *DiscordAdapter) discordBotsRequireInlineMention(ctx context.Context) bool {
	if configured, ok := a.Config.Extra["bots_require_inline_mention"].(string); ok {
		switch strings.ToLower(configured) {
		case "true", "1", "yes", "on":
			return true
		}
		return false
	}
	return a.extraOrEnvFlag(ctx, "bots_require_inline_mention", "DISCORD_BOTS_REQUIRE_INLINE_MENTION", "true", true)
}

// Channel keys of a message accepted by channel gates
func (a *DiscordAdapter) discordChannelKeys(m *inboundMessage, parentChannelID string) strSet {
	return a.discordChannelKeysFromChannel(m.channel, parentChannelID)
}

// Channel id, bare and hashed names, plus parent id and names for threads
func (a *DiscordAdapter) discordChannelKeysFromChannel(ch *discordgo.Channel, parentChannelID string) strSet {
	keys := strSet{}
	if ch == nil {
		return keys
	}
	if ch.ID != "" {
		keys[ch.ID] = struct{}{}
	}
	if name := pystr.Strip(ch.Name); name != "" {
		keys[name] = struct{}{}
		keys["#"+name] = struct{}{}
	}
	parentID := parentChannelID
	if parentID == "" {
		parentID = ch.ParentID
	}
	if parentID != "" {
		keys[parentID] = struct{}{}
	}
	if parent := a.stateChannel(ch.ParentID); parent != nil {
		if parentName := pystr.Strip(parent.Name); parentName != "" {
			keys[parentName] = struct{}{}
			keys["#"+parentName] = struct{}{}
		}
	}
	return keys
}

// Whether threads still require a mention after the bot participated, off by default
func (a *DiscordAdapter) discordThreadRequireMention(ctx context.Context) bool {
	return a.extraOrEnvFlag(ctx, "thread_require_mention", "DISCORD_THREAD_REQUIRE_MENTION", "false", true)
}

// Whether history backfill is enabled for shared sessions
func (a *DiscordAdapter) discordHistoryBackfill(ctx context.Context) bool {
	return a.extraOrEnvFlag(ctx, "history_backfill", "DISCORD_HISTORY_BACKFILL", "true", true)
}

// Most messages scanned backwards by history backfill, 50 by default
func (a *DiscordAdapter) discordHistoryBackfillLimit(ctx context.Context) int {
	if configured, ok := a.Config.Extra["history_backfill_limit"]; ok && configured != nil {
		if n, err := pyint.Of(configured); err == nil && n.IsInt64() {
			return int(n.Int64())
		}
	}
	raw := gateway.PlatformGateEnv(ctx, "DISCORD_HISTORY_BACKFILL_LIMIT", "50")
	if n, ok := pyint.Parse(raw); ok && n.IsInt64() {
		return int(n.Int64())
	}
	return 50
}
