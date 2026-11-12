package discord

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Events the adapter handed to handle_message
type eventCapture struct {
	mu     sync.Mutex
	events []*gateway.MessageEvent
}

func (c *eventCapture) all() []*gateway.MessageEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*gateway.MessageEvent(nil), c.events...)
}

// Adapter of the free response fixture, bot user 999, batching off and handle_message recorded
func inboundAdapter(t *testing.T) (*DiscordAdapter, *fakeDiscordAPI, *eventCapture) {
	t.Helper()
	for _, v := range []string{"DISCORD_REQUIRE_MENTION", "DISCORD_THREAD_REQUIRE_MENTION", "DISCORD_FREE_RESPONSE_CHANNELS",
		"DISCORD_FREE_RESPONSE_AUTO_THREAD", "DISCORD_AUTO_THREAD", "DISCORD_NO_THREAD_CHANNELS", "DISCORD_ALLOWED_CHANNELS",
		"DISCORD_IGNORED_CHANNELS", "DISCORD_HISTORY_BACKFILL", "DISCORD_HISTORY_BACKFILL_LIMIT", "DISCORD_ALLOW_BOTS"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := newTestAdapter(t, nil)
	s := attachClient(t, a, "999", []string{"g1"})
	f := newFakeDiscordAPI(t, s)
	a.textBatchDelaySeconds = 0
	a.SetTextBatchDelays(0, a.textBatchSplitDelaySeconds)
	c := &eventCapture{}
	a.handleMessage = func(_ context.Context, ev *gateway.MessageEvent) error {
		c.mu.Lock()
		c.events = append(c.events, ev)
		c.mu.Unlock()
		return nil
	}
	a.readyEvent.Set()
	orig := autoThreadRetryDelay
	autoThreadRetryDelay = 0
	t.Cleanup(func() { autoThreadRetryDelay = orig })
	return a, f, c
}

// Guild text channel cached in the client state
func textChannel(t *testing.T, a *DiscordAdapter, id string) *discordgo.Channel {
	ch := &discordgo.Channel{ID: id, GuildID: "g1", Name: "general", Type: discordgo.ChannelTypeGuildText}
	if err := a.client.State.ChannelAdd(ch); err != nil {
		t.Fatal(err)
	}
	return ch
}

// Public thread under a parent channel cached in the client state
func threadChannel(t *testing.T, a *DiscordAdapter, id, parent string) *discordgo.Channel {
	ch := &discordgo.Channel{ID: id, GuildID: "g1", Name: "thread", ParentID: parent, Type: discordgo.ChannelTypeGuildPublicThread}
	if err := a.client.State.ChannelAdd(ch); err != nil {
		t.Fatal(err)
	}
	return ch
}

// Direct message channel
func dmChannel(id string) *discordgo.Channel {
	return &discordgo.Channel{ID: id, Type: discordgo.ChannelTypeDM}
}

// Message 123 from Jezza in a channel, the mentioned users attached
func makeMessage(ch *discordgo.Channel, content string, mentions ...*discordgo.User) *inboundMessage {
	return &inboundMessage{Message: &discordgo.Message{ID: "123", ChannelID: ch.ID, GuildID: ch.GuildID, Content: content,
		Author: &discordgo.User{ID: "42", Username: "Jezza", GlobalName: "Jezza"}, Mentions: mentions, Type: discordgo.MessageTypeDefault}, channel: ch}
}

// History message by an author
func historyMessage(author *discordgo.User, content, id string) *discordgo.Message {
	return &discordgo.Message{ID: id, Author: author, Content: content, Type: discordgo.MessageTypeDefault}
}
