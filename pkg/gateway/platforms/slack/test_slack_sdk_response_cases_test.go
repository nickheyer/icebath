package slack

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func TestSlackResponsePayloadPlainDictPassesThrough(t *testing.T) {
	payload := map[string]any{"ok": true, "ts": "1.0"}
	got := slackResponsePayload(payload)
	got["marker"] = 1
	if payload["marker"] != 1 {
		t.Fatal("plain dict was copied instead of passed through")
	}
}

func TestSlackResponsePayloadSDKResponseYieldsItsData(t *testing.T) {
	got := slackResponsePayload(&webResponse{Data: map[string]any{"ok": true}})
	if !reflect.DeepEqual(got, map[string]any{"ok": true}) {
		t.Fatalf("got %v", got)
	}
}

func TestSlackResponsePayloadBinaryResponseIsNotMistakenForData(t *testing.T) {
	if got := slackResponsePayload(&webResponse{Data: []byte("\x89PNG")}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestSlackResponsePayloadUnknownShapeYieldsEmpty(t *testing.T) {
	if got := slackResponsePayload(struct{}{}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if got := slackResponsePayload(nil); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func writeReport(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(p, []byte("%PDF-1.4 x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStandaloneSendMediaPathTextAlongsideMediaReportsItsTs(t *testing.T) {
	f := newFakeSlackAPI(t)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "111.222"})
	result, _ := standaloneSend(context.Background(), map[string]any{
		"pconfig": &gateway.PlatformConfig{Token: "xoxb-test", Extra: map[string]any{}}, "chat_id": "C_GEN",
		"message": "Here is the report", "media_files": []gateway.MediaRef{{Path: writeReport(t)}},
	})
	if result["success"] != true || result["message_id"] != "111.222" {
		t.Fatalf("result %v", result)
	}
}

func TestStandaloneSendMediaPathTextPostErrorIsSurfaced(t *testing.T) {
	f := newFakeSlackAPI(t)
	f.respond("chat.postMessage", map[string]any{"ok": false, "error": "channel_not_found"})
	result, _ := standaloneSend(context.Background(), map[string]any{
		"pconfig": &gateway.PlatformConfig{Token: "xoxb-test", Extra: map[string]any{}}, "chat_id": "C_GEN",
		"message": "Here is the report", "media_files": []gateway.MediaRef{{Path: writeReport(t)}},
	})
	if !strings.Contains(pyrepr.Str(result["error"]), "channel_not_found") {
		t.Fatalf("result %v", result)
	}
	if n := len(f.callsTo("files.completeUploadExternal")) + len(f.callsTo("files.getUploadURLExternal")); n != 0 {
		t.Fatalf("upload attempted %d times", n)
	}
}

func TestStandaloneSendMediaPathCaptionWithUnfurlControlsPostsTextSeparately(t *testing.T) {
	f := newFakeSlackAPI(t)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "111.222"})
	result, _ := standaloneSend(context.Background(), map[string]any{
		"pconfig": &gateway.PlatformConfig{Token: "xoxb-test", Extra: map[string]any{"unfurl_links": false, "unfurl_media": false}},
		"chat_id": "C_GEN", "message": "", "media_files": []gateway.MediaRef{{Path: writeReport(t)}},
		"caption": "[Report](https://example.com/report)",
	})
	if result["success"] != true {
		t.Fatalf("result %v", result)
	}
	post := f.callsTo("chat.postMessage")[0].Args
	if post["text"] != "<https://example.com/report|Report>" || post["unfurl_links"] != false || post["unfurl_media"] != false {
		t.Fatalf("post %v", post)
	}
	if c := f.callsTo("files.completeUploadExternal")[0].Args; c["initial_comment"] != "" {
		t.Fatalf("complete %v", c)
	}
}

func TestStandaloneSendMediaPathCaptionFallbackDeliversWhenMediaIsMissing(t *testing.T) {
	f := newFakeSlackAPI(t)
	f.respond("chat.postMessage", map[string]any{"ok": true, "ts": "555.666"})
	result, _ := standaloneSend(context.Background(), map[string]any{
		"pconfig": &gateway.PlatformConfig{Token: "xoxb-test", Extra: map[string]any{}}, "chat_id": "C_GEN", "message": "",
		"media_files": []gateway.MediaRef{{Path: filepath.Join(t.TempDir(), "gone.pdf")}}, "caption": "Here is the report",
	})
	if result["success"] != true || result["message_id"] != "555.666" {
		t.Fatalf("result %v", result)
	}
	posts := f.callsTo("chat.postMessage")
	if len(posts) != 1 || posts[0].Args["text"] != "Here is the report" {
		t.Fatalf("posts %v", posts)
	}
}
