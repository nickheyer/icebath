package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Inbound message with its channel resolved from the client cache
type inboundMessage struct {
	*discordgo.Message
	channel *discordgo.Channel
}

// Python isoformat of a timestamp
func isoformat(t time.Time) string { return pydatetime.ISOFormat(t) }

// Cached channel of an id, nil when the client or the entry is absent
func (a *DiscordAdapter) stateChannel(id string) *discordgo.Channel {
	if id == "" {
		return nil
	}
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil || client.State == nil {
		return nil
	}
	ch, err := client.State.Channel(id)
	if err != nil {
		return nil
	}
	return ch
}

// Channel of a message from the cache, a bare DM or guild text channel when uncached
func (a *DiscordAdapter) channelOf(m *discordgo.Message) *discordgo.Channel {
	if ch := a.stateChannel(m.ChannelID); ch != nil {
		return ch
	}
	if m.ChannelID == "" {
		return nil
	}
	if m.GuildID == "" {
		return &discordgo.Channel{ID: m.ChannelID, Type: discordgo.ChannelTypeDM}
	}
	return &discordgo.Channel{ID: m.ChannelID, GuildID: m.GuildID, Type: discordgo.ChannelTypeGuildText}
}

// Wraps a gateway message with its resolved channel
func (a *DiscordAdapter) inbound(m *discordgo.Message) *inboundMessage {
	return &inboundMessage{Message: m, channel: a.channelOf(m)}
}

// Whether the channel is a one to one DM
func isDMChannel(ch *discordgo.Channel) bool {
	return ch != nil && ch.Type == discordgo.ChannelTypeDM
}

// Parent channel id of a thread like channel, empty when it has none
func (a *DiscordAdapter) getParentChannelID(ch *discordgo.Channel) string {
	if ch == nil {
		return ""
	}
	return ch.ParentID
}

// Whether a channel is a forum channel
func isForumParent(ch *discordgo.Channel) bool {
	return ch != nil && ch.Type == discordgo.ChannelTypeGuildForum
}

// Channel topic, falling back to the parent forum topic for forum threads
func (a *DiscordAdapter) getEffectiveTopic(ch *discordgo.Channel, isThread bool) string {
	if ch == nil {
		return ""
	}
	topic := ch.Topic
	if topic == "" && isThread {
		if parent := a.stateChannel(ch.ParentID); parent != nil && isForumParent(parent) {
			topic = parent.Topic
		}
	}
	return topic
}

// Whether the channel ids intersect DISCORD_ALLOWED_CHANNELS
func (a *DiscordAdapter) discordChannelIDsAllowed(ctx context.Context, channelIDs strSet) bool {
	if len(channelIDs) == 0 {
		return false
	}
	allowed := a.getAllowedChannels(ctx)
	if len(allowed) == 0 {
		return false
	}
	if allowed.has("*") {
		return true
	}
	return channelIDs.intersects(allowed)
}

// Whether the Discord user holds an explicit pairing grant
func (a *DiscordAdapter) isPairingApprovedUser(ctx context.Context, userID string) bool {
	userID = pystr.Strip(userID)
	if userID == "" {
		return false
	}
	store, err := gateway.NewPairingStore(ctx, "")
	if err != nil {
		return false
	}
	ok, err := store.IsApproved(ctx, "discord", userID)
	return err == nil && ok
}

// Roles a message author carries in the guild the member record came from
type authorRoles struct {
	roles   []string
	guildID string
}

// Allows by allowed users or roles, else by validated channel context, with DM role checks scoped to one guild
func (a *DiscordAdapter) isAllowedUser(ctx context.Context, userID string, author *authorRoles, guildID string, isDM bool, channelIDs strSet) bool {
	a.mu.Lock()
	allowedUsers, allowedRoles := a.allowedUserIDs, a.allowedRoleIDs
	a.mu.Unlock()
	hasUsers, hasRoles := len(allowedUsers) > 0, len(allowedRoles) > 0
	if a.isPairingApprovedUser(ctx, userID) {
		return true
	}
	if !hasUsers && !hasRoles {
		if a.discordAllowAllUsers(ctx) || a.gatewayAllowAllUsers(ctx) {
			return true
		}
		return !isDM && channelIDs != nil && a.discordChannelIDsAllowed(ctx, channelIDs)
	}
	if hasUsers && (allowedUsers.has("*") || allowedUsers.has(userID)) {
		return true
	}
	if !hasRoles {
		return false
	}
	if isDM || guildID == "" {
		dmGuildID, err := ReadDMRoleAuthGuild(ctx)
		if err != nil || dmGuildID == nil {
			return false
		}
		a.mu.Lock()
		client := a.client
		a.mu.Unlock()
		if client == nil || client.State == nil {
			return false
		}
		if _, err := client.State.Guild(dmGuildID.String()); err != nil {
			return false
		}
		return a.guildMemberHasRole(dmGuildID.String(), userID, allowedRoles)
	}
	if author != nil && len(author.roles) > 0 && (author.guildID == "" || author.guildID == guildID) {
		for _, r := range author.roles {
			if allowedRoles.has(r) {
				return true
			}
		}
	}
	return a.guildMemberHasRole(guildID, userID, allowedRoles)
}

// Whether the user, looked up as a member of this guild only, holds an allowed role
func (a *DiscordAdapter) guildMemberHasRole(guildID, userID string, allowedRoles strSet) bool {
	uid, err := strconv.ParseUint(pystr.Strip(userID), 10, 64)
	if err != nil {
		return false
	}
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil || client.State == nil {
		return false
	}
	m, err := client.State.Member(guildID, strconv.FormatUint(uid, 10))
	if err != nil || m == nil {
		return false
	}
	for _, r := range m.Roles {
		if allowedRoles.has(r) {
			return true
		}
	}
	return false
}

// Logs once when Discord traffic is denied because no allowlist is configured
func (a *DiscordAdapter) warnIfFailClosedDefault(ctx context.Context) {
	a.mu.Lock()
	warned, users, roles := a.warnedFailClosedDefault, len(a.allowedUserIDs), len(a.allowedRoleIDs)
	a.mu.Unlock()
	if warned || users > 0 || roles > 0 {
		return
	}
	if len(a.getAllowedChannels(ctx)) > 0 || a.discordAllowAllUsers(ctx) || a.gatewayAllowAllUsers(ctx) {
		return
	}
	a.mu.Lock()
	a.warnedFailClosedDefault = true
	a.mu.Unlock()
	slog.Warn(fmt.Sprintf("[%s] Discord messages are being denied because no allowlist is configured. "+
		"Set DISCORD_ALLOWED_USERS, DISCORD_ALLOWED_ROLES, or DISCORD_ALLOWED_CHANNELS, or set DISCORD_ALLOW_ALL_USERS=true for open access.", a.Name()))
}

// Whether the message sits in a thread the bot joined that needs no mention
func (a *DiscordAdapter) inBotThread(ctx context.Context, m *inboundMessage) bool {
	return m.channel != nil && m.channel.IsThread() && a.threads.Contains(m.channel.ID) && !a.discordThreadRequireMention(ctx)
}

// Admission verdict and role authorization of one Discord event under dedup, author, bot and mention gates
func (a *DiscordAdapter) discordMessageAdmission(ctx context.Context, m *inboundMessage, claim bool) (bool, bool) {
	if claim {
		if a.dedup.IsDuplicate(m.ID) {
			return false, false
		}
	} else if a.dedup.Contains(m.ID) {
		return false, false
	}
	me := a.selfUser()
	if m.Author != nil && me != nil && m.Author.ID == me.ID {
		return false, false
	}
	if m.Type != discordgo.MessageTypeDefault && m.Type != discordgo.MessageTypeReply {
		return false, false
	}
	roleAuthorized := false
	if m.Author != nil && m.Author.Bot {
		allowBots := a.getAllowBots(ctx)
		continuation := a.isBotTagDebounceContinuation(m)
		if allowBots == "none" {
			return false, false
		}
		if allowBots == "mentions" && !a.selfIsExplicitlyMentioned(m) && !continuation {
			return false, false
		}
		if a.discordBotsRequireInlineMention(ctx) && !a.selfIsRawMentioned(m) && !continuation {
			return false, false
		}
	} else {
		isDM := isDMChannel(m.channel) || m.GuildID == ""
		var channelIDs strSet
		if !isDM {
			channelIDs = newStrSet(m.ChannelID)
			if parentID := a.getParentChannelID(m.channel); parentID != "" {
				channelIDs[parentID] = struct{}{}
			}
		}
		userID := ""
		if m.Author != nil {
			userID = m.Author.ID
		}
		var roles *authorRoles
		if m.Member != nil {
			roles = &authorRoles{roles: m.Member.Roles, guildID: m.Member.GuildID}
		}
		if !a.isAllowedUser(ctx, userID, roles, m.GuildID, isDM, channelIDs) {
			a.warnIfFailClosedDefault(ctx)
			return false, false
		}
		a.mu.Lock()
		roleAuthorized = len(a.allowedRoleIDs) > 0
		a.mu.Unlock()
	}
	rawSelfMention := a.selfIsExplicitlyMentioned(m)
	if !isDMChannel(m.channel) && (len(m.Mentions) > 0 || rawSelfMention) {
		otherBotsMentioned := false
		for _, u := range m.Mentions {
			if u != nil && u.Bot && (me == nil || u.ID != me.ID) {
				otherBotsMentioned = true
				break
			}
		}
		if otherBotsMentioned && !rawSelfMention {
			return false, false
		}
		switch strings.ToLower(gateway.PlatformGateEnv(ctx, "DISCORD_IGNORE_NO_MENTION", "true")) {
		case "true", "1", "yes":
			if !rawSelfMention && !otherBotsMentioned && !a.inBotThread(ctx, m) {
				parentID := ""
				if m.channel != nil {
					parentID = m.channel.ParentID
				}
				free := a.discordFreeResponseChannels(ctx)
				if !free.has("*") && !a.discordChannelKeys(m, parentID).intersects(free) {
					slog.Debug(fmt.Sprintf("[%s] admission: dropping message %s — mentions others, not self, not a bot thread, channel not free-response", a.Name(), m.ID))
					return false, false
				}
			}
		}
	}
	return true, roleAuthorized
}
