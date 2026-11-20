package discord

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// Adapter admitting every human sender with the given bot mode and extra config
func inlineMentionAdapter(t *testing.T, allowBots string, extra map[string]any) *DiscordAdapter {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("DISCORD_BOTS_REQUIRE_INLINE_MENTION", "")
	t.Setenv("DISCORD_ALLOW_BOTS", "")
	if extra == nil {
		extra = map[string]any{}
	}
	extra["allow_bots"] = allowBots
	a := newTestAdapter(t, extra)
	a.allowedUserIDs = newStrSet("*")
	a.textBatchDelaySeconds, a.textBatchSplitDelaySeconds = 0.6, 2.0
	attachClient(t, a, "99", nil)
	return a
}

// Bot reply whose mention list carries this bot without a typed token
func replyPingFromBot(a *DiscordAdapter, content string) *inboundMessage {
	return &inboundMessage{Message: &discordgo.Message{ID: "123", ChannelID: "7", Content: content, Type: discordgo.MessageTypeReply,
		Author: &discordgo.User{ID: "42", Bot: true}, Mentions: []*discordgo.User{a.selfUser()}},
		channel: &discordgo.Channel{ID: "7", Type: discordgo.ChannelTypeGuildText}}
}

func TestReplyPingAloneDoesNotTriggerBotByDefault(t *testing.T) {
	for _, allowBots := range []string{"mentions", "all"} {
		t.Run(allowBots, func(t *testing.T) {
			a := inlineMentionAdapter(t, allowBots, nil)
			if ok, _ := a.discordMessageAdmission(context.Background(), replyPingFromBot(a, "reply without a typed mention"), false); ok {
				t.Fatal("reply ping admitted")
			}
			if ok, _ := a.discordMessageAdmission(context.Background(), replyPingFromBot(a, "<@99> intentional handoff"), false); !ok {
				t.Fatal("typed mention rejected")
			}
		})
	}
}

func TestExplicitFalseRestoresReplyPingCompatibility(t *testing.T) {
	a := inlineMentionAdapter(t, "mentions", map[string]any{"bots_require_inline_mention": false})
	if ok, _ := a.discordMessageAdmission(context.Background(), replyPingFromBot(a, "legacy reply handoff"), false); !ok {
		t.Fatal("legacy reply rejected")
	}
}
