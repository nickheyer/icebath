package discord

import (
	"context"
	"errors"
	"strconv"

	"github.com/bwmarrin/discordgo"
)

// Display name of a cached guild member, nil when the client, guild or member is not cached
func (a *DiscordAdapter) GuildMemberDisplayName(guildID, userID int64) any {
	client := a.liveClient()
	if client == nil || client.State == nil {
		return nil
	}
	m, err := client.State.Member(strconv.FormatInt(guildID, 10), strconv.FormatInt(userID, 10))
	if err != nil || m == nil || m.User == nil {
		return nil
	}
	if name := memberDisplayName(m); name != "" {
		return name
	}
	return nil
}

// Global or user name of a user the client cache holds in any guild, nil when none does
func (a *DiscordAdapter) CachedUserDisplayName(userID int64) any {
	client := a.liveClient()
	if client == nil || client.State == nil {
		return nil
	}
	id := strconv.FormatInt(userID, 10)
	if self := client.State.User; self != nil && self.ID == id {
		return userDisplayName(self)
	}
	client.State.RLock()
	defer client.State.RUnlock()
	for _, g := range client.State.Guilds {
		for _, m := range g.Members {
			if m.User != nil && m.User.ID == id {
				return userDisplayName(m.User)
			}
		}
	}
	return nil
}

// User display name as discord.py's User.display_name gives it, nil when empty
func userDisplayName(u *discordgo.User) any {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	if u.Username != "" {
		return u.Username
	}
	return nil
}

// Cached guild of an id, nil when absent
func (a *DiscordAdapter) CachedGuild(guildID int64) any {
	if g := a.stateGuild(strconv.FormatInt(guildID, 10)); g != nil {
		return g
	}
	return nil
}

// Cached channel of an id, nil when absent
func (a *DiscordAdapter) CachedChannel(channelID int64) any {
	if ch := a.stateChannel(strconv.FormatInt(channelID, 10)); ch != nil {
		return ch
	}
	return nil
}

// Posts text to a cached channel with the adapter's allowed mentions
func (a *DiscordAdapter) SendToChannel(ctx context.Context, channel any, content string) error {
	ch, ok := channel.(*discordgo.Channel)
	if !ok || ch == nil {
		return errors.New("AttributeError: object has no attribute 'send'")
	}
	_, err := a.channelSend(ctx, ch.ID, content, nil)
	return err
}

// Thread id and chat id of a channel, the thread id set only for threads
func (a *DiscordAdapter) ThreadIDAndChatForChannel(channel any) (*string, any) {
	ch, ok := channel.(*discordgo.Channel)
	if !ok || ch == nil || ch.ID == "" {
		return nil, nil
	}
	id := ch.ID
	if ch.IsThread() {
		return &id, id
	}
	return nil, id
}

// Parent channel id of a thread like channel, nil when it has none
func (a *DiscordAdapter) GetParentChannelID(channel any) *string {
	ch, ok := channel.(*discordgo.Channel)
	if !ok || ch == nil {
		return nil
	}
	if parent := a.getParentChannelID(ch); parent != "" {
		return &parent
	}
	return nil
}

// Chat name and topic a message posted in the guild channel or thread carries
func (a *DiscordAdapter) GuildChannelLabels(channel any) (name, topic *string, ok bool) {
	ch, isChannel := channel.(*discordgo.Channel)
	if !isChannel || ch == nil {
		return nil, nil, false
	}
	var label, t string
	if ch.IsThread() {
		label, t = a.formatThreadChatName(ch), a.getEffectiveTopic(ch, true)
	} else {
		label = ch.Name
		if g := a.stateGuild(ch.GuildID); g != nil {
			label = g.Name + " / #" + ch.Name
		}
		t = a.getEffectiveTopic(ch, false)
	}
	name = &label
	if t != "" {
		topic = &t
	}
	return name, topic, true
}

// Allowed role ids of the adapter
func (a *DiscordAdapter) AllowedRoleIDs() map[string]struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]struct{}, len(a.allowedRoleIDs))
	for k := range a.allowedRoleIDs {
		out[k] = struct{}{}
	}
	return out
}

// Whether the user passes the user and role allow lists against a cached guild
func (a *DiscordAdapter) IsAllowedUser(userID string, guild any, isDM bool) any {
	guildID := ""
	if g, ok := guild.(*discordgo.Guild); ok && g != nil {
		guildID = g.ID
	}
	return a.isAllowedUser(a.voiceTaskContext(), userID, nil, guildID, isDM, nil)
}
