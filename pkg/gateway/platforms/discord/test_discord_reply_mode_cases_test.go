package discord

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// Adapter in the given reply mode whose splitter returns the fixed chunks
func replyModeAdapter(t *testing.T, mode string, chunks []string) (*DiscordAdapter, *fakeDiscordAPI) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := newTestAdapter(t, nil)
	a.replyToMode = mode
	s := attachClient(t, a, "1", []string{"999"}, &discordgo.Channel{ID: "12345", GuildID: "999", Type: discordgo.ChannelTypeGuildText})
	prev := truncateMessage
	truncateMessage = func(string, int, func(string) int) []string { return chunks }
	t.Cleanup(func() { truncateMessage = prev })
	return a, newFakeDiscordAPI(t, s)
}

func TestSendWithReplyToModeOffModeNoReplyReference(t *testing.T) {
	a, api := replyModeAdapter(t, "off", []string{"chunk1", "chunk2", "chunk3"})
	a.Send(context.Background(), "12345", "test content", "999", nil)
	if len(api.matching("GET", "/messages/999")) != 0 {
		t.Fatal("reply target fetched")
	}
	for _, s := range api.sends("12345") {
		if s["message_reference"] != nil {
			t.Fatal("off mode sent a reference")
		}
	}
}

func TestSendWithReplyToModeSingleChunkOffMode(t *testing.T) {
	a, api := replyModeAdapter(t, "off", []string{"single chunk"})
	a.Send(context.Background(), "12345", "test", "999", nil)
	sends := api.sends("12345")
	if len(api.matching("GET", "/messages/999")) != 0 || len(sends) != 1 || sends[0]["message_reference"] != nil {
		t.Fatalf("sends %v", sends)
	}
}

func TestSendWithReplyToModeFirstModeConstructsReferenceWithoutFetch(t *testing.T) {
	a, api := replyModeAdapter(t, "first", []string{"chunk1", "chunk2"})
	a.Send(context.Background(), "12345", "test content", "999", nil)
	sends := api.sends("12345")
	if len(api.matching("GET", "/messages/999")) != 0 || len(sends) != 2 || sends[0]["message_reference"] == nil || sends[1]["message_reference"] != nil {
		t.Fatalf("sends %v", sends)
	}
}
