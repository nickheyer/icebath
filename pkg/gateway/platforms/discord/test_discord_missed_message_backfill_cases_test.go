package discord

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Adapter with backfill on, bot user 999 and channel 123 answering history with the given messages
func backfillAdapter(t *testing.T, history string, channels ...*discordgo.Channel) *DiscordAdapter {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("DISCORD_MISSED_MESSAGE_BACKFILL", "true")
	t.Setenv("DISCORD_ALLOW_ALL_USERS", "true")
	a := newTestAdapter(t, nil)
	s := attachClient(t, a, "999", []string{"777"}, channels...)
	api := newFakeDiscordAPI(t, s)
	api.mu.Lock()
	api.get = map[string]string{"/channels/123/messages": history, "/channels/456/messages": history}
	api.mu.Unlock()
	return a
}

func backfillMessage(id, authorID string, bot bool, content string, ch *discordgo.Channel, mentions ...*discordgo.User) *inboundMessage {
	return &inboundMessage{Message: &discordgo.Message{ID: id, ChannelID: ch.ID, GuildID: "777", Content: content, Timestamp: time.Now().UTC(),
		Author: &discordgo.User{ID: authorID, Bot: bot}, Mentions: mentions, Type: discordgo.MessageTypeDefault}, channel: ch}
}

var backfillChannel = &discordgo.Channel{ID: "123", GuildID: "777", Name: "wiki-inbox", Type: discordgo.ChannelTypeGuildText}

func TestConfiguredBotSenderIsLeftForSharedIngressPolicy(t *testing.T) {
	a := backfillAdapter(t, `[]`, backfillChannel)
	t.Setenv("DISCORD_ALLOW_BOTS", "mentions")
	m := backfillMessage("98", "42", true, "<@999> run this", backfillChannel, a.selfUser())
	if !a.shouldBackfillDiscordMessage(context.Background(), m) {
		t.Fatal("bot sender skipped")
	}
}

func TestShouldNotBackfillMessageWithNonDownBotResponse(t *testing.T) {
	a := backfillAdapter(t, `[{"id":"2","content":"Done — captured it.","author":{"id":"999","bot":true},"message_reference":{"message_id":"1"}}]`, backfillChannel)
	if a.shouldBackfillDiscordMessage(context.Background(), backfillMessage("1", "42", false, "please ingest", backfillChannel)) {
		t.Fatal("answered message backfilled")
	}
}

func TestParentChannelUnreferencedBotMessageDoesNotSuppressBackfill(t *testing.T) {
	a := backfillAdapter(t, `[{"id":"2","content":"Done — captured a different item.","author":{"id":"999","bot":true}}]`, backfillChannel)
	if !a.shouldBackfillDiscordMessage(context.Background(), backfillMessage("1", "42", false, "please ingest", backfillChannel)) {
		t.Fatal("unreferenced post suppressed backfill")
	}
}

func TestThreadUnreferencedBotMessageDoesNotMaskRequest(t *testing.T) {
	thread := &discordgo.Channel{ID: "456", ParentID: "123", GuildID: "777", Type: discordgo.ChannelTypeGuildPublicThread}
	a := backfillAdapter(t, `[{"id":"2","content":"Done — captured a different request.","author":{"id":"999","bot":true}}]`, backfillChannel, thread)
	if !a.shouldBackfillDiscordMessage(context.Background(), backfillMessage("1", "42", false, "please ingest", thread)) {
		t.Fatal("thread post masked the request")
	}
}

func TestBackfillsWhenOnlyDownNoticeExists(t *testing.T) {
	a := backfillAdapter(t, `[{"id":"2","content":"The agent is down right now.","author":{"id":"999","bot":true},"message_reference":{"message_id":"1"}}]`, backfillChannel)
	if !a.shouldBackfillDiscordMessage(context.Background(), backfillMessage("1", "42", false, "please ingest", backfillChannel)) {
		t.Fatal("down notice counted as an answer")
	}
}

func TestGenericUnavailableResponseCountsAsCompleted(t *testing.T) {
	a := backfillAdapter(t, `[{"id":"2","content":"That package is unavailable on this platform.","author":{"id":"999","bot":true},"message_reference":{"message_id":"1"}}]`, backfillChannel)
	if a.shouldBackfillDiscordMessage(context.Background(), backfillMessage("1", "42", false, "please ingest", backfillChannel)) {
		t.Fatal("generic reply not counted")
	}
}
