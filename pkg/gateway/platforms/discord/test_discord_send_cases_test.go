package discord

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
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

// Reports a fake size for one path the way the hermes case patches os.path.getsize
func fakeGetsize(t *testing.T, oversized string, size int64) {
	prev := getsize
	t.Cleanup(func() { getsize = prev })
	getsize = func(path string) (int64, error) {
		if path == oversized {
			return size, nil
		}
		return prev(path)
	}
}

func TestSendVideoUsesPathBasedFilesKwarg(t *testing.T) {
	a, api := sendAdapter(t)
	video := filepath.Join(t.TempDir(), "clip.mp4")
	os.WriteFile(video, []byte("\x00\x00\x00\x18ftypmp42fake"), 0o644)
	res, err := a.SendVideo(context.Background(), "555", video, "", "", nil, nil)
	if err != nil || !res.Success || res.MessageID == "" {
		t.Fatalf("send = %+v %v", res, err)
	}
	sends := api.sends("555")
	if len(sends) != 1 || sends[0]["_files"] != 1 || sends[0]["_filenames"].([]any)[0] != "clip.mp4" ||
		sends[0]["_filedata"].([]string)[0] != "\x00\x00\x00\x18ftypmp42fake" {
		t.Fatalf("sends = %v", sends)
	}
}

func TestSendVideoFailsLoudWhenMessageHasNoAttachments(t *testing.T) {
	a, api := sendAdapter(t)
	video := filepath.Join(t.TempDir(), "clip.mp4")
	os.WriteFile(video, []byte("fake-mp4"), 0o644)
	api.setFail(func(c fakeCall, _ int) (int, string) {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/messages") {
			return http.StatusOK, `{"id":"99","channel_id":"555","attachments":[]}`
		}
		return 0, ""
	})
	res, _ := a.SendVideo(context.Background(), "555", video, "", "", nil, nil)
	if res.Success || !strings.Contains(strings.ToLower(fmt.Sprint(res.Error)), "no files") || len(api.sends("555")) != 1 {
		t.Fatalf("send = %+v", res)
	}
}

func TestSendVideoMissingFileFailsFastWithoutTouchingChannel(t *testing.T) {
	a, api := sendAdapter(t)
	res, _ := a.SendVideo(context.Background(), "555", "/no/such/clip.mp4", "", "", nil, nil)
	if res.Success || !strings.Contains(strings.ToLower(fmt.Sprint(res.Error)), "not found") || len(api.matching("", "")) != 0 {
		t.Fatalf("send = %+v calls %v", res, api.matching("", ""))
	}
}

func TestSendFileAttachmentForumUsesFilesKwarg(t *testing.T) {
	a, api := sendAdapter(t)
	a.client.State.ChannelAdd(&discordgo.Channel{ID: "7", GuildID: "999", Type: discordgo.ChannelTypeGuildForum})
	api.thread = func(body map[string]any) string {
		return `{"id":"7001","type":11,"parent_id":"7","message":{"id":"8","channel_id":"7001","attachments":[{"id":"1","filename":"clip.mp4"}]}}`
	}
	video := filepath.Join(t.TempDir(), "clip.mp4")
	os.WriteFile(video, []byte("fake-mp4"), 0o644)
	res, _ := a.SendVideo(context.Background(), "7", video, "", "", nil, nil)
	posts := api.matching(http.MethodPost, "/channels/7/threads")
	if !res.Success || len(posts) != 1 || posts[0].Body["_files"] != 1 {
		t.Fatalf("send = %+v posts %v", res, posts)
	}
}

func TestDiscordUploadLimitUsesGuildFilesizeLimit(t *testing.T) {
	a, _ := sendAdapter(t)
	limitFor := func(tier discordgo.PremiumTier, guild bool) int {
		id := fmt.Sprintf("g%d%v", tier, guild)
		if guild {
			a.client.State.GuildAdd(&discordgo.Guild{ID: id, PremiumTier: tier})
		}
		return a.discordUploadLimitBytes(&discordgo.Channel{ID: "1", GuildID: id})
	}
	if limitFor(discordgo.PremiumTier2, true) != 50*1024*1024 || limitFor(discordgo.PremiumTierNone, true) != discordDefaultUploadLimitBytes ||
		limitFor(discordgo.PremiumTierNone, false) != discordDefaultUploadLimitBytes || a.discordUploadLimitBytes(&discordgo.Channel{ID: "1"}) != discordDefaultUploadLimitBytes {
		t.Fatal("upload limit does not follow the guild cap floored at the default")
	}
}

func TestOversizedUploadRejectedBeforeSend(t *testing.T) {
	for _, c := range []struct{ method, name string }{{"send_video", "clip.mp4"}, {"send_document", "report.pdf"}, {"send_voice", "note.ogg"}} {
		t.Run(c.method, func(t *testing.T) {
			a, api := sendAdapter(t)
			big := filepath.Join(t.TempDir(), c.name)
			os.WriteFile(big, []byte("x"), 0o644)
			fakeGetsize(t, big, discordDefaultUploadLimitBytes+1)
			ctx := context.Background()
			var res gateway.SendResult
			switch c.method {
			case "send_video":
				res, _ = a.SendVideo(ctx, "555", big, "", "", nil, nil)
			case "send_document":
				res, _ = a.SendDocument(ctx, "555", big, "", "", "", nil, nil)
			case "send_voice":
				res, _ = a.SendVoice(ctx, "555", big, "", "", nil, nil)
			}
			errText := fmt.Sprint(res.Error)
			if res.Success || !strings.Contains(strings.ToLower(errText), "too large") || !strings.Contains(errText, c.name) ||
				!strings.Contains(errText, "20.0 MB") || !strings.Contains(errText, "limit 20 MB") {
				t.Fatalf("send = %+v", res)
			}
			sends := api.sends("555")
			if len(sends) != 1 || sends[0]["_files"] != nil || !strings.Contains(sends[0]["content"].(string), "Could not attach") ||
				!strings.Contains(sends[0]["content"].(string), c.name) {
				t.Fatalf("sends = %v", sends)
			}
		})
	}
}

func TestSendVideoUnderGuildBoostLimitUploads(t *testing.T) {
	a, api := sendAdapter(t)
	a.client.State.GuildAdd(&discordgo.Guild{ID: "888", PremiumTier: discordgo.PremiumTier2})
	a.client.State.ChannelAdd(&discordgo.Channel{ID: "777", GuildID: "888", Type: discordgo.ChannelTypeGuildText})
	video := filepath.Join(t.TempDir(), "ok.mp4")
	os.WriteFile(video, []byte("fake-video-bytes"), 0o644)
	fakeGetsize(t, video, discordDefaultUploadLimitBytes+1)
	res, _ := a.SendVideo(context.Background(), "777", video, "", "", nil, nil)
	if sends := api.sends("777"); !res.Success || len(sends) != 1 || sends[0]["_files"] != 1 {
		t.Fatalf("send = %+v sends %v", res, sends)
	}
}

func TestSendMultipleImagesSkipsOversizedLocalFile(t *testing.T) {
	a, api := sendAdapter(t)
	dir := t.TempDir()
	small, big := filepath.Join(dir, "small.png"), filepath.Join(dir, "big.png")
	os.WriteFile(small, []byte("ok"), 0o644)
	os.WriteFile(big, []byte("x"), 0o644)
	fakeGetsize(t, big, discordDefaultUploadLimitBytes+1)
	ctx := context.Background()
	mixed, _ := a.SendMultipleImages(ctx, "555", []gateway.ImageRef{{URL: "file://" + small}, {URL: "file://" + big}}, nil, 0)
	sends := api.sends("555")
	if !mixed.Success || len(sends) != 1 || sends[0]["_files"] != 1 || !strings.Contains(fmt.Sprint(sends[0]["content"]), "big.png") ||
		!strings.Contains(fmt.Sprint(sends[0]["content"]), "exceeds") {
		t.Fatalf("mixed = %+v sends %v", mixed, sends)
	}
	onlyBig, _ := a.SendMultipleImages(ctx, "555", []gateway.ImageRef{{URL: "file://" + big}}, nil, 0)
	sends = api.sends("555")
	if onlyBig.Success || len(sends) != 2 || sends[1]["_files"] != nil || !strings.Contains(fmt.Sprint(sends[1]["content"]), "big.png") {
		t.Fatalf("only big = %+v sends %v", onlyBig, sends)
	}
}
