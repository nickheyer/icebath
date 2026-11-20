package discord

import (
	"context"
	"strconv"

	"github.com/bwmarrin/discordgo"
)

// Snowflake bounding a timestamp from above, the way history after a datetime is anchored
func timeSnowflakeHigh(m *discordgo.Message) string {
	if m.Timestamp.IsZero() {
		return "0"
	}
	millis := m.Timestamp.UnixMilli() - 1420070400000
	return strconv.FormatUint(uint64(millis)<<22+(1<<22-1), 10)
}

// Whether a bot reply referencing the message, other than an outage notice, follows it in the channel
func (a *DiscordAdapter) messageHasNonDownBotResponse(ctx context.Context, m *inboundMessage) bool {
	me := a.selfUser()
	client := a.liveClient()
	if me == nil || client == nil {
		return false
	}
	scan := func(channelID string) bool {
		if channelID == "" {
			return false
		}
		history, err := client.ChannelMessages(channelID, 25, "", timeSnowflakeHigh(m.Message), "", discordgo.WithContext(ctx))
		if err != nil {
			return false
		}
		for _, candidate := range history {
			if candidate.Author == nil || candidate.Author.ID != me.ID || isDownNoticeContent(candidate.Content) {
				continue
			}
			if candidate.MessageReference != nil && candidate.MessageReference.MessageID == m.ID {
				return true
			}
		}
		return false
	}
	if m.channel != nil && scan(m.channel.ID) {
		return true
	}
	return m.Thread != nil && scan(m.Thread.ID)
}

// Whether a recent message still needs agent work after an outage
func (a *DiscordAdapter) shouldBackfillDiscordMessage(ctx context.Context, m *inboundMessage) bool {
	me := a.selfUser()
	if a.liveClient() == nil || me == nil {
		return false
	}
	if m.Author != nil && m.Author.ID == me.ID {
		return false
	}
	if a.discordMessageIsPersistentlyComplete(ctx, m.ID) || a.discordMessageHasActiveClaim(ctx, m.ID) || a.discordMessageAttemptsExhausted(ctx, m.ID) {
		return false
	}
	return !a.messageHasNonDownBotResponse(ctx, m)
}
