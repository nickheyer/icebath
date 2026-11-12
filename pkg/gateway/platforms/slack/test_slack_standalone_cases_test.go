package slack

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func clearSlackDMCache() {
	slackDMCacheMu.Lock()
	slackDMCache.Clear()
	slackDMCacheMu.Unlock()
}

func writePNG(t *testing.T, name string) string {
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("\x89PNG\r\n\x1a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStandaloneSendMediaUploadsLocalMediaWithMessageAsCaption(t *testing.T) {
	f := newFakeSlackAPI(t)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "1.0"})
	image := writePNG(t, "daily-report.png")
	result, _ := standaloneSend(context.Background(), map[string]any{
		"pconfig": &gateway.PlatformConfig{Enabled: true, Token: "xoxb-fake-token"}, "chat_id": "C123",
		"message": "daily report", "thread_id": nil, "media_files": []gateway.MediaRef{{Path: image}},
	})
	if result["success"] != true {
		t.Fatalf("result %v", result)
	}
	posts := f.callsTo("chat.postMessage")
	if len(posts) != 1 || posts[0].Args["text"] != "daily report" {
		t.Fatalf("posts %v", posts)
	}
	urls := f.callsTo("files.getUploadURLExternal")
	completes := f.callsTo("files.completeUploadExternal")
	if len(urls) != 1 || len(completes) != 1 {
		t.Fatalf("uploads %v %v", urls, completes)
	}
	if urls[0].Args["filename"] != "daily-report.png" || completes[0].Args["channel_id"] != "C123" || completes[0].Args["initial_comment"] != "" {
		t.Fatalf("upload args %v %v", urls[0].Args, completes[0].Args)
	}
}

func TestStandaloneSendUserDmResolutionChannelIDSkipsResolution(t *testing.T) {
	clearSlackDMCache()
	f := newFakeSlackAPI(t)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "123.456"})
	result, _ := standaloneSend(context.Background(), map[string]any{
		"pconfig": &gateway.PlatformConfig{Enabled: true, Token: "xoxb-fake-token"}, "chat_id": "C123", "message": "hello channel",
	})
	if result["success"] != true {
		t.Fatalf("result %v", result)
	}
	calls := f.allCalls()
	if len(calls) != 1 || calls[0].Method != "chat.postMessage" {
		t.Fatalf("calls %v", calls)
	}
}

func TestStandaloneSendUserDmResolutionChannelDeliveryHonorsUnfurlConfig(t *testing.T) {
	clearSlackDMCache()
	f := newFakeSlackAPI(t)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "123.456"})
	result, _ := standaloneSend(context.Background(), map[string]any{
		"pconfig": &gateway.PlatformConfig{Enabled: true, Token: "xoxb-fake-token", Extra: map[string]any{"unfurl_links": false, "unfurl_media": false}},
		"chat_id": "C123", "message": "[Hermes](https://example.com/hermes)",
	})
	if result["success"] != true {
		t.Fatalf("result %v", result)
	}
	payload := f.callsTo("chat.postMessage")[0].Args
	if payload["text"] != "<https://example.com/hermes|Hermes>" || payload["unfurl_links"] != false || payload["unfurl_media"] != false {
		t.Fatalf("payload %v", payload)
	}
}

func TestStandaloneSendUserDmResolutionUserIDMediaDeliveryResolvesDmBeforeUpload(t *testing.T) {
	clearSlackDMCache()
	t.Cleanup(clearSlackDMCache)
	f := newFakeSlackAPI(t)
	f.respond("conversations.open", map[string]any{"ok": true, "channel": map[string]any{"id": "D777666555"}})
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "1.0"})
	image := writePNG(t, "report.png")
	result, _ := standaloneSend(context.Background(), map[string]any{
		"pconfig": &gateway.PlatformConfig{Enabled: true, Token: "xoxb-fake-token"}, "chat_id": "U1234509876",
		"message": "daily report", "media_files": []gateway.MediaRef{{Path: image}},
	})
	if result["success"] != true || result["chat_id"] != "D777666555" {
		t.Fatalf("result %v", result)
	}
	completes := f.callsTo("files.completeUploadExternal")
	if len(completes) != 1 || completes[0].Args["channel_id"] != "D777666555" {
		t.Fatalf("completes %v", completes)
	}
}
