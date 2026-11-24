package discord

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

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

// Context whose settings reads resolve in the profile this adapter serves
func (a *DiscordAdapter) sctx(ctx context.Context) context.Context {
	a.mu.Lock()
	h := a.settingsHome
	a.mu.Unlock()
	if h != "" {
		return home.WithOverride(ctx, h)
	}
	return gateway.SettingsContext(ctx)
}

// Whether Discord channel messages require a bot mention
func (a *DiscordAdapter) discordRequireMention(ctx context.Context) bool {
	return settings.DiscordRequireMention.Get(a.sctx(ctx))
}

// Whether free response channels also auto thread, off by default
func (a *DiscordAdapter) discordFreeResponseAutoThread(ctx context.Context) bool {
	return settings.DiscordFreeResponseAutoThread.Get(a.sctx(ctx))
}

// Per attachment byte cap, zero unlimited
func (a *DiscordAdapter) discordMaxAttachmentBytes(ctx context.Context) int64 {
	return max(int64(settings.DiscordMaxAttachmentBytes.Get(a.sctx(ctx))), 0)
}

// Whether a Discord audio attachment is a native voice note
func isDiscordVoiceMessageAttachment(att *discordgo.MessageAttachment) bool {
	return att != nil && att.Waveform != ""
}

// Binds the adapter to the home of the profile the connecting context serves, so later reads use that profile's settings
func (a *DiscordAdapter) bindProfile(ctx context.Context) {
	h := home.Dir(gateway.SettingsContext(ctx))
	a.mu.Lock()
	a.settingsHome = h
	a.mu.Unlock()
}

// Items of a list setting of this adapter's profile as a set
func (a *DiscordAdapter) settingSet(ctx context.Context, l *settings.List) strSet {
	out := strSet{}
	for _, item := range l.Strings(gateway.SettingsContext(a.sctx(ctx))) {
		if item = strings.TrimSpace(item); item != "" {
			out[item] = struct{}{}
		}
	}
	return out
}

// Channels this adapter answers in, empty for every channel
func (a *DiscordAdapter) getAllowedChannels(ctx context.Context) strSet {
	return a.settingSet(ctx, settings.DiscordAllowedChannels)
}

// Channels this adapter never reads
func (a *DiscordAdapter) getIgnoredChannels(ctx context.Context) strSet {
	return a.settingSet(ctx, settings.DiscordIgnoredChannels)
}

// Channels this adapter answers in without opening a thread
func (a *DiscordAdapter) getNoThreadChannels(ctx context.Context) strSet {
	return a.settingSet(ctx, settings.DiscordNoThreadChannels)
}

// Cleaned allowed people entries of this adapter's profile
func (a *DiscordAdapter) getAllowedUsers(ctx context.Context) strSet {
	out := strSet{}
	for entry := range a.settingSet(ctx, settings.DiscordAllowFrom) {
		if cleaned := cleanDiscordID(entry); cleaned != "" {
			out[cleaned] = struct{}{}
		}
	}
	return out
}

// Allowed role ids of this adapter's profile in canonical decimal form
func (a *DiscordAdapter) getAllowedRoles(ctx context.Context) (strSet, error) {
	return canonicalRoleIDs(a.settingSet(ctx, settings.DiscordAllowedRoles))
}

// Text of the allowed people and allowed roles settings of this adapter's profile
func (a *DiscordAdapter) allowlistSettingsText(ctx context.Context) [2]string {
	sctx := a.sctx(ctx)
	return [2]string{gateway.SettingText(sctx, settings.DiscordAllowFrom), gateway.SettingText(sctx, settings.DiscordAllowedRoles)}
}

// Takes up a change of the allowed people or allowed roles settings made since they were last read, resolving usernames anew
func (a *DiscordAdapter) syncAllowlists(ctx context.Context) {
	stamp := a.allowlistSettingsText(ctx)
	a.mu.Lock()
	unchanged := a.allowlistStamp == stamp
	a.mu.Unlock()
	if unchanged {
		return
	}
	users := a.getAllowedUsers(ctx)
	roles, err := a.getAllowedRoles(ctx)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[%s] %s does not read, so no role is allowed until it is fixed: %s", a.Name(), settings.DiscordAllowedRoles.Key, err))
		roles = strSet{}
	}
	a.mu.Lock()
	a.allowedUserIDs, a.allowedRoleIDs, a.allowlistStamp = users, roles, stamp
	a.mu.Unlock()
	if client := a.liveClient(); client != nil && client.Identify.Intents&discordgo.IntentGuildMembers == 0 && needsServerMembersIntent(keysOf(users), keysOf(roles)) {
		pkgLog().Warn(fmt.Sprintf("[%s] The allowed people or roles now need Discord's Server Members Intent, which this connection was opened without. "+
			"Restart the background service so usernames resolve and roles count in direct messages.", a.Name()))
	}
	a.resolveAllowedUsernames(ctx)
}

// Role ids of a set in canonical decimal form, entries that are not ids skipped
func canonicalRoleIDs(entries strSet) (strSet, error) {
	out := strSet{}
	for entry := range entries {
		entry = strings.TrimSpace(entry)
		if !isDigitString(entry) {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(entry), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid literal for int() with base 10: %s", anyutil.Quote(entry))
		}
		out[strconv.FormatInt(n, 10)] = struct{}{}
	}
	return out, nil
}

// Numeric ids the allowed people entries name without spelling them plainly, resolved usernames and pasted mentions, read by the access checks
func (a *DiscordAdapter) ResolvedAllowlistUserIDs() (any, error) {
	a.mu.Lock()
	ctx := a.runCtx
	a.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	a.syncAllowlists(ctx)
	ids := strSet{}
	for entry := range a.settingSet(ctx, settings.DiscordAllowFrom) {
		if cleaned := cleanDiscordID(entry); cleaned != entry && isDigitString(cleaned) {
			ids[cleaned] = struct{}{}
		}
	}
	a.mu.Lock()
	for id := range a.usernameResolvedIDs {
		ids[id] = struct{}{}
	}
	a.mu.Unlock()
	return slices.Sorted(maps.Keys(ids)), nil
}

// Whether this adapter's profile lets everyone talk to it on Discord
func (a *DiscordAdapter) discordAllowAllUsers(ctx context.Context) bool {
	return settings.DiscordAllowAllUsers.Get(a.sctx(ctx))
}

// Whether this adapter's profile lets everyone talk to it on every chat app
func (a *DiscordAdapter) gatewayAllowAllUsers(ctx context.Context) bool {
	return settings.GatewayAllowAllUsers.Get(a.sctx(ctx))
}

// Which messages from other bots this adapter's profile answers, none, mentions or all
func (a *DiscordAdapter) getAllowBots(ctx context.Context) string {
	return settings.DiscordAllowBots.Get(a.sctx(ctx))
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
	return a.settingSet(ctx, settings.DiscordFreeResponseChannels)
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

// Whether another bot must type an inline mention to trigger this bot
func (a *DiscordAdapter) discordBotsRequireInlineMention(ctx context.Context) bool {
	return settings.DiscordBotsRequireInlineMention.Get(a.sctx(ctx))
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
	if name := strings.TrimSpace(ch.Name); name != "" {
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
		if parentName := strings.TrimSpace(parent.Name); parentName != "" {
			keys[parentName] = struct{}{}
			keys["#"+parentName] = struct{}{}
		}
	}
	return keys
}

// Whether threads still require a mention after the bot participated
func (a *DiscordAdapter) discordThreadRequireMention(ctx context.Context) bool {
	return settings.DiscordThreadRequireMention.Get(a.sctx(ctx))
}

// Whether history backfill is enabled for shared sessions
func (a *DiscordAdapter) discordHistoryBackfill(ctx context.Context) bool {
	return settings.DiscordHistoryBackfill.Get(a.sctx(ctx))
}

// Most messages scanned backwards by history backfill
func (a *DiscordAdapter) discordHistoryBackfillLimit(ctx context.Context) int {
	return settings.DiscordHistoryBackfillLimit.Get(a.sctx(ctx))
}
