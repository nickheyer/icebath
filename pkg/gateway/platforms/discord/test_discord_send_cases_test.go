package discord

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestSendRejectsWhitespaceAndRecordsFailedFinalReply(t *testing.T) {
	a, api := sendAdapter(t)
	t.Setenv("DISCORD_MISSED_MESSAGE_BACKFILL", "true")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	result, _ := a.Send(context.Background(), "555", "  \n\t ", "123", map[string]any{"notify": true})
	if result.Success || result.Error != "Refusing to send empty message" {
		t.Fatalf("result %+v", result)
	}
	if len(api.sends("555")) != 0 {
		t.Fatal("empty message sent")
	}
	row := a.withDiscordRecoveryDB(context.Background(), func(conn state.SQLiteConn) (any, error) {
		var status string
		var replied, outage int
		var resp sql.NullString
		err := conn.QueryRowContext(context.Background(), "SELECT status, replied, outage_response, response_message_id FROM discord_messages WHERE message_id='123'").Scan(&status, &replied, &outage, &resp)
		return []any{status, replied, outage, resp.Valid}, err
	}, nil).([]any)
	if row[0] != "failed" || row[1] != 0 || row[2] != 0 || row[3] != false {
		t.Fatalf("row %v", row)
	}
	if !strings.Contains(buf.String(), "Dropped empty message to chat=555") {
		t.Fatal("no warning")
	}
}

func TestSendRetriesWithoutReferenceWhenReplyTargetIsDeleted(t *testing.T) {
	a, api := sendAdapter(t)
	api.mu.Lock()
	api.fail = func(c fakeCall, n int) (int, string) {
		if c.Method == http.MethodPost && n == 1 {
			return 400, `{"message":"Unknown Message","code":10008}`
		}
		return 0, ""
	}
	api.mu.Unlock()
	result, _ := a.Send(context.Background(), "555", strings.Repeat("A", discordMaxMessageLength+50), "99", nil)
	if !result.Success || result.MessageID != "9001" {
		t.Fatalf("result %+v", result)
	}
	sends := api.sends("555")
	if len(sends) != 3 {
		t.Fatalf("sends %d", len(sends))
	}
	ref, _ := sends[0]["message_reference"].(map[string]any)
	if ref == nil || ref["message_id"] != "99" || ref["fail_if_not_exists"] != false {
		t.Fatalf("first reference %v", sends[0]["message_reference"])
	}
	if sends[1]["message_reference"] != nil {
		t.Fatal("retry kept the reference")
	}
}

func TestForumPostFileCreatesThreadWithAttachment(t *testing.T) {
	a, api := sendAdapter(t)
	api.mu.Lock()
	api.thread = func(map[string]any) string {
		return `{"id":"777","type":11,"message":{"id":"800","attachments":[{"id":"1","filename":"photo.png"}]}}`
	}
	api.mu.Unlock()
	forum := &discordgo.Channel{ID: "999", Name: "ideas", Type: discordgo.ChannelTypeGuildForum}
	result := a.forumPostFile(context.Background(), forum, "", "here is a photo", []*discordgo.File{{Name: "photo.png", Reader: strings.NewReader("png")}})
	if !result.Success || result.MessageID != "800" {
		t.Fatalf("result %+v", result)
	}
	calls := api.matching(http.MethodPost, "/channels/999/threads")
	if len(calls) != 1 {
		t.Fatalf("thread creates %d", len(calls))
	}
	body := calls[0].Body
	msg, _ := body["message"].(map[string]any)
	if body["name"] != "here is a photo" || msg["content"] != "here is a photo" || body["_files"] != 1 {
		t.Fatalf("payload %v", body)
	}
}

func TestForumPostFileFailsWhenStarterHasNoAttachments(t *testing.T) {
	a, api := sendAdapter(t)
	api.mu.Lock()
	api.thread = func(map[string]any) string { return `{"id":"7","type":11,"message":{"id":"8","attachments":[]}}` }
	api.mu.Unlock()
	forum := &discordgo.Channel{ID: "999", Type: discordgo.ChannelTypeGuildForum}
	result := a.forumPostFile(context.Background(), forum, "", "video clip", []*discordgo.File{{Name: "clip.mp4", Reader: strings.NewReader("mp4")}})
	if result.Success || !strings.Contains(strings.ToLower(result.Error.(string)), "no files") {
		t.Fatalf("result %+v", result)
	}
	if len(api.matching(http.MethodPost, "/channels/999/threads")) != 1 {
		t.Fatal("thread not created once")
	}
}

func TestTypingRestartableAfterError(t *testing.T) {
	a, api := sendAdapter(t)
	api.mu.Lock()
	api.fail = func(c fakeCall, n int) (int, string) {
		if n == 1 {
			return 503, `{"message":"503"}`
		}
		return 0, ""
	}
	api.mu.Unlock()
	a.SendTyping(context.Background(), "12345", nil)
	time.Sleep(100 * time.Millisecond)
	a.SendTyping(context.Background(), "12345", nil)
	a.mu.Lock()
	_, running := a.typingTasks["12345"]
	a.mu.Unlock()
	if !running {
		t.Fatal("Should restart typing after previous failure")
	}
	a.StopTyping(context.Background(), "12345", nil)
}
