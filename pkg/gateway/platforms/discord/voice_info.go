package discord

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Binds a join's transcription text channel and source, dropping old speech and source when the text channel moves
func (a *DiscordAdapter) bindVoiceTextChannel(guildID int64, textChannelID *int64, source map[string]any) {
	if textChannelID != nil {
		a.mu.Lock()
		previous, hasPrevious := a.voiceTextChannels[guildID]
		a.mu.Unlock()
		if hasPrevious && previous != *textChannelID {
			a.DiscardPendingVoiceInput(guildID)
			if source == nil {
				a.mu.Lock()
				delete(a.voiceSources, guildID)
				a.mu.Unlock()
			}
		}
		a.mu.Lock()
		a.voiceTextChannels[guildID] = *textChannelID
		a.mu.Unlock()
	}
	if source != nil {
		a.mu.Lock()
		a.voiceSources[guildID] = source
		a.mu.Unlock()
	}
}

// Name, members, member count and speaking users of the bot's voice channel, nil when not connected
func (a *DiscordAdapter) GetVoiceChannelInfo(guildID int64) map[string]any {
	a.mu.Lock()
	vc := a.voiceClients[guildID]
	receiver := a.voiceReceivers[guildID]
	a.mu.Unlock()
	if vc == nil || !vc.IsConnected() {
		return nil
	}
	channel := vc.Channel()
	if channel == nil {
		return nil
	}
	bot := a.selfUser()
	members := []map[string]any{}
	for _, m := range vc.ChannelMembers() {
		if bot != nil && m.User.ID == bot.ID {
			continue
		}
		id, _ := strconv.ParseInt(m.User.ID, 10, 64)
		members = append(members, map[string]any{"user_id": id, "display_name": memberDisplayName(m), "is_bot": m.User.Bot})
	}
	speaking := map[int64]bool{}
	if receiver != nil {
		speaking = receiver.SpeakingUserIDs(2 * time.Second)
	}
	for _, info := range members {
		info["is_speaking"] = speaking[info["user_id"].(int64)]
	}
	return map[string]any{"channel_name": channel.Name, "member_count": len(members), "members": members, "speaking_count": len(speaking)}
}

// Prompt context naming the voice channel and its participants, empty when not connected
func (a *DiscordAdapter) GetVoiceChannelContext(guildID int64) (string, error) {
	info := a.GetVoiceChannelInfo(guildID)
	if len(info) == 0 {
		return "", nil
	}
	parts := []string{fmt.Sprintf("[Voice channel: #%s — %d participant(s)]", info["channel_name"], info["member_count"])}
	for _, m := range info["members"].([]map[string]any) {
		status := ""
		if m["is_speaking"].(bool) {
			status = " (speaking)"
		}
		parts = append(parts, fmt.Sprintf("  - %s%s", m["display_name"], status))
	}
	return strings.Join(parts, "\n"), nil
}
