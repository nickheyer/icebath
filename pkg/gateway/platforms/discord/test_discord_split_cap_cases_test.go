package discord

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

func hugeContent() string {
	var parts []string
	for i := 0; i < 60000/20; i++ {
		parts = append(parts, fmt.Sprintf("word-%d-", i)+strings.Repeat("x", 12))
	}
	return strings.Join(parts, " ")
}

func TestCapSplitChunksBelowCapUnchanged(t *testing.T) {
	a, _ := sendAdapter(t)
	chunks := []string{"a", "b", "c"}
	if got := a.capSplitChunks(context.Background(), chunks); !slices.Equal(got, chunks) {
		t.Fatalf("got %v", got)
	}
}

func TestCapSplitChunksOverCapKeepsNMinus1PlusNotice(t *testing.T) {
	a, _ := sendAdapter(t)
	var chunks []string
	for i := 0; i < 40; i++ {
		chunks = append(chunks, fmt.Sprintf("chunk-%d-", i)+strings.Repeat("z", 100))
	}
	capped := a.capSplitChunks(context.Background(), chunks)
	if len(capped) != discordMaxSplitMessages || capped[0] != chunks[0] || slices.Contains(chunks, capped[len(capped)-1]) ||
		utf8.RuneCountInString(capped[len(capped)-1]) > discordMaxMessageLength {
		t.Fatalf("capped %d", len(capped))
	}
}

func TestSendCapSendCapsSplitFlood(t *testing.T) {
	a, api := sendAdapter(t)
	result, _ := a.Send(context.Background(), "555", hugeContent(), "", nil)
	sends := api.sentContents("555")
	if !result.Success || len(sends) != discordMaxSplitMessages || !strings.Contains(sends[len(sends)-1], "Response truncated") {
		t.Fatalf("result %+v sends %d", result, len(sends))
	}
}

func TestForumCapSendToForumCapsFollowupChunks(t *testing.T) {
	a, api := sendAdapter(t)
	forum := &discordgo.Channel{ID: "666", Type: discordgo.ChannelTypeGuildForum}
	result := a.sendToForum(context.Background(), forum, hugeContent())
	sends := api.sentContents("777")
	if !result.Success || len(sends) > discordMaxSplitMessages-1 || !strings.Contains(sends[len(sends)-1], "Response truncated") {
		t.Fatalf("result %+v sends %d", result, len(sends))
	}
}

func TestEditOverflowCapEditOverflowSplitCapped(t *testing.T) {
	a, api := sendAdapter(t)
	ch := &discordgo.Channel{ID: "555", GuildID: "999"}
	result := a.editOverflowSplit(context.Background(), ch, "42", "42", hugeContent())
	edits, sends := api.editContents("555", "42"), api.sentContents("555")
	last := ""
	if len(sends) > 0 {
		last = sends[len(sends)-1]
	} else {
		last = edits[len(edits)-1]
	}
	if !result.Success || len(edits) != 1 || len(sends) > discordMaxSplitMessages-1 || !strings.Contains(last, "Response truncated") {
		t.Fatalf("result %+v edits %d sends %d", result, len(edits), len(sends))
	}
}
