package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Channel of an interaction from the client cache, a bare DM or guild channel when uncached
func (a *DiscordAdapter) interactionChannel(i *discordgo.Interaction) *discordgo.Channel {
	if i == nil || i.ChannelID == "" {
		return nil
	}
	if ch := a.stateChannel(i.ChannelID); ch != nil {
		return ch
	}
	if i.GuildID == "" {
		return &discordgo.Channel{ID: i.ChannelID, Type: discordgo.ChannelTypeDM}
	}
	return &discordgo.Channel{ID: i.ChannelID, GuildID: i.GuildID, Type: discordgo.ChannelTypeGuildText}
}

// Acting user of an interaction, the member user in guilds and the raw user in DMs
func interactionUser(i *discordgo.Interaction) *discordgo.User {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User
	}
	return i.User
}

// Slash verdict and refusal reason under the same channel, user and role gates as messages
func (a *DiscordAdapter) evaluateSlashAuthorization(ctx context.Context, i *discordgo.Interaction) (bool, string) {
	ch := a.interactionChannel(i)
	inDM := ch != nil && ch.Type == discordgo.ChannelTypeDM
	channelIDs, channelKeys := strSet{}, strSet{}
	if !inDM {
		chanID := i.ChannelID
		if chanID == "" && ch != nil {
			chanID = ch.ID
		}
		isThread := ch != nil && ch.IsThread()
		parentID := ""
		if isThread {
			parentID = a.getParentChannelID(ch)
		}
		if chanID != "" {
			channelIDs[chanID] = struct{}{}
			if parentID != "" {
				channelIDs[parentID] = struct{}{}
			}
		}
		channelKeys = a.discordChannelKeysFromChannel(ch, parentID)
		if allowed := a.getAllowedChannels(ctx); len(allowed) > 0 && !allowed.has("*") {
			if len(channelIDs) == 0 {
				return false, "channel id missing with DISCORD_ALLOWED_CHANNELS configured"
			}
			if !channelKeys.intersects(allowed) {
				return false, "channel not in DISCORD_ALLOWED_CHANNELS"
			}
		}
		if ignored := a.getIgnoredChannels(ctx); len(ignored) > 0 && len(channelIDs) > 0 {
			if ignored.has("*") || channelKeys.intersects(ignored) {
				return false, "channel in DISCORD_IGNORED_CHANNELS"
			}
		}
	}
	user := interactionUser(i)
	a.mu.Lock()
	hasLists := len(a.allowedUserIDs) > 0 || len(a.allowedRoleIDs) > 0
	a.mu.Unlock()
	if user == nil || user.ID == "" {
		if hasLists {
			return false, "missing interaction.user with allowlist configured"
		}
		return false, "missing interaction.user"
	}
	var roles *authorRoles
	if i.Member != nil {
		roles = &authorRoles{roles: i.Member.Roles, guildID: i.GuildID}
	}
	var keys strSet
	if !inDM {
		keys = channelKeys
	}
	if !a.isAllowedUser(ctx, user.ID, roles, i.GuildID, inDM, keys) {
		return false, "user not in DISCORD_ALLOWED_USERS / DISCORD_ALLOWED_ROLES"
	}
	return true, ""
}

// Role grant of an event built from the interaction, set only when a role list exists and the actor passes
func (a *DiscordAdapter) slashRoleGrant(ctx context.Context, i *discordgo.Interaction) bool {
	a.mu.Lock()
	hasRoles := len(a.allowedRoleIDs) > 0
	a.mu.Unlock()
	if !hasRoles {
		return false
	}
	ok, _ := a.evaluateSlashAuthorization(ctx, i)
	return ok
}

// Chat type, chat id and thread id of a click, labelled the way message intake labels the channel
func (a *DiscordAdapter) componentChatContext(i *discordgo.Interaction) (string, *string, *string) {
	var chatID *string
	if i.ChannelID != "" {
		chatID = strPtr(i.ChannelID)
	}
	if i.GuildID == "" {
		return "dm", chatID, nil
	}
	if ch := a.interactionChannel(i); ch != nil && ch.IsThread() {
		return "thread", chatID, chatID
	}
	return "group", chatID, nil
}

// Gateway's live allowlist verdict for a click, nil when no check is wired
func (a *DiscordAdapter) componentLiveAuth(i *discordgo.Interaction) *bool {
	userID := ""
	if u := interactionUser(i); u != nil {
		userID = u.ID
	}
	chatType, chatID, threadID := a.componentChatContext(i)
	return a.IsSenderAuthorized(userID, &chatType, chatID, gateway.AuthorizationOpts{ThreadID: threadID})
}

// Whether a gate env flag reads as an allow all switch
func truthyGate(v string) bool {
	switch strings.ToLower(pystr.Strip(v)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// User or role OR authorization of a component click, snapshot users confirmed by the live check
func (a *DiscordAdapter) componentCheckAuth(ctx context.Context, i *discordgo.Interaction, allowedUserIDs, allowedRoleIDs strSet, liveAuth func(*discordgo.Interaction) *bool) bool {
	user := interactionUser(i)
	if user == nil || user.ID == "" {
		return false
	}
	if truthyGate(gateway.PlatformGateEnv(ctx, "DISCORD_ALLOW_ALL_USERS", "")) || truthyGate(gateway.PlatformGateEnv(ctx, "GATEWAY_ALLOW_ALL_USERS", "")) {
		return true
	}
	userSet := strSet{}
	for uid := range allowedUserIDs {
		if s := pystr.Strip(uid); s != "" {
			userSet[s] = struct{}{}
		}
	}
	for _, uid := range strings.Split(gateway.PlatformGateEnv(ctx, "GATEWAY_ALLOWED_USERS", ""), ",") {
		if s := pystr.Strip(uid); s != "" {
			userSet[s] = struct{}{}
		}
	}
	uid := user.ID
	if len(userSet) > 0 && (userSet.has("*") || userSet.has(uid)) {
		if liveAuth == nil {
			return true
		}
		if verdict := liveAuth(i); verdict == nil || *verdict {
			return true
		}
	}
	if len(allowedRoleIDs) > 0 {
		if i.Member == nil {
			return false
		}
		for _, r := range i.Member.Roles {
			if allowedRoleIDs.has(r) {
				return true
			}
		}
	}
	return uid != "" && a.isPairingApprovedUser(ctx, uid)
}

// Parent text channel of a thread, the channel itself otherwise
func (a *DiscordAdapter) threadParentChannel(ch *discordgo.Channel) *discordgo.Channel {
	if ch != nil {
		if parent := a.stateChannel(ch.ParentID); parent != nil {
			return parent
		}
	}
	return ch
}

// Channel an interaction came from, cached first then fetched, nil when unknown
func (a *DiscordAdapter) resolveInteractionChannel(ctx context.Context, i *discordgo.Interaction) *discordgo.Channel {
	client := a.liveClient()
	if client == nil || i.ChannelID == "" {
		return nil
	}
	if ch := a.stateChannel(i.ChannelID); ch != nil {
		return ch
	}
	ch, err := client.Channel(i.ChannelID, discordgo.WithContext(ctx))
	if err != nil {
		return nil
	}
	return ch
}

// Defers the interaction ephemerally, false after a warning when its token already expired
func (a *DiscordAdapter) deferUnlessExpired(ctx context.Context, i *discordgo.Interaction, warnFmt string, warnArgs ...any) (bool, error) {
	client := a.liveClient()
	if client == nil {
		return false, errors.New("Not connected")
	}
	err := client.InteractionRespond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral}}, discordgo.WithContext(ctx))
	if err == nil {
		return true, nil
	}
	if !isDiscordUnknownInteraction(err) {
		return false, dpyError(err)
	}
	slog.Warn(fmt.Sprintf(warnFmt, warnArgs...))
	return false, nil
}
