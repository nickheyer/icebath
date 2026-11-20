package discord

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// Adapter that joined thread 555 with no free response channels
func threadExemptionAdapter(t *testing.T, threadRequireMention bool) *DiscordAdapter {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("DISCORD_IGNORE_NO_MENTION", "")
	t.Setenv("DISCORD_FREE_RESPONSE_CHANNELS", "")
	t.Setenv("DISCORD_THREAD_REQUIRE_MENTION", "")
	a := newTestAdapter(t, map[string]any{"thread_require_mention": threadRequireMention})
	a.allowedUserIDs = newStrSet("*")
	attachClient(t, a, "99", nil)
	a.threads.Mark(context.Background(), "555")
	return a
}

// Human message in the channel mentioning a third party only
func mentionsThirdParty(ch *discordgo.Channel) *inboundMessage {
	return &inboundMessage{Message: &discordgo.Message{ID: "123", ChannelID: ch.ID, GuildID: "1", Content: "<@77> test",
		Type: discordgo.MessageTypeDefault, Author: &discordgo.User{ID: "42"}, Mentions: []*discordgo.User{{ID: "77"}}}, channel: ch}
}

func TestThirdPartyMentionInBotThreadIsAdmitted(t *testing.T) {
	thread := &discordgo.Channel{ID: "555", Type: discordgo.ChannelTypeGuildPublicThread}
	if ok, _ := threadExemptionAdapter(t, false).discordMessageAdmission(context.Background(), mentionsThirdParty(thread), false); !ok {
		t.Fatal("bot thread mention dropped")
	}
	if ok, _ := threadExemptionAdapter(t, true).discordMessageAdmission(context.Background(), mentionsThirdParty(thread), false); ok {
		t.Fatal("thread_require_mention overridden")
	}
}

func TestThirdPartyMentionInAPlainChannelIsStillDroppedAndLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	plain := &discordgo.Channel{ID: "555", Type: discordgo.ChannelTypeGuildText}
	if ok, _ := threadExemptionAdapter(t, false).discordMessageAdmission(context.Background(), mentionsThirdParty(plain), false); ok {
		t.Fatal("plain channel mention admitted")
	}
	if !strings.Contains(buf.String(), "admission: dropping message 123") {
		t.Fatalf("no drop log in %q", buf.String())
	}
}
