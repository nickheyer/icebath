package gateway

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Telegram descriptor of the media cases, send_media advertised unless ops are given
func relayMediaDesc(ops ...string) func(d *CapabilityDescriptor) {
	return func(d *CapabilityDescriptor) {
		d.SupportedOps = []string{"send", "edit", "typing", "get_chat_info", "send_media"}
		if len(ops) > 0 {
			d.SupportedOps = ops
		}
	}
}

// Adapter over a scripted connector whose media client uploads to a loopback media endpoint answering id aa11
func relayMediaAdapter(t *testing.T, mod func(d *CapabilityDescriptor)) (*RelayAdapter, *relayCapture, func() []http.Header, string) {
	t.Helper()
	a, c := relayConnectorAdapter(t, mod)
	srv, uploads := relayMediaServer(t, []byte(`{"id": "aa11"}`), nil)
	a.mediaClient = NewRelayMediaClient(srv.URL, "gw-1", strings.Repeat("s", 64))
	return a, c, uploads, srv.URL + "/relay/media/aa11"
}

// Writes the bytes to a file of the name under a fresh temp dir
func relayTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func relayOpsOf(c *relayCapture) []any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []any
	for _, a := range c.sent {
		out = append(out, a["op"])
	}
	return out
}

func TestSendImageURLPassesThroughWithoutUpload(t *testing.T) {
	a, c, uploads, _ := relayMediaAdapter(t, relayMediaDesc())
	r, err := a.SendImage(context.Background(), "chat1", "https://fal.media/x.png", "a pic", "m9", nil)
	action, _ := c.last(t)
	if err != nil || !r.Success || r.MessageID != "md1" || len(uploads()) != 0 || action["op"] != "send_media" || action["media_kind"] != "image" ||
		action["source_url"] != "https://fal.media/x.png" || action["content"] != "a pic" || action["reply_to"] != "m9" {
		t.Fatalf("result %+v err %v action %v", r, err, action)
	}
}

func TestLocalPathLanesUploadFirst(t *testing.T) {
	a, c, uploads, ref := relayMediaAdapter(t, relayMediaDesc())
	f := relayTempFile(t, "clip.ogg", []byte("oggbytes"))
	r, err := a.SendVoice(context.Background(), "chat1", f, "listen", "", nil, nil)
	action, _ := c.last(t)
	ups := uploads()
	if err != nil || !r.Success || len(ups) != 1 || ups[0].Get("X-Media-Filename") != "clip.ogg" || action["op"] != "send_media" ||
		action["media_kind"] != "voice" || action["source_url"] != ref || strings.Contains(relayReprOf(action), f) {
		t.Fatalf("result %+v err %v action %v uploads %v", r, err, action, ups)
	}
}

func TestOpGatingFallsBackWhenNotAdvertised(t *testing.T) {
	a, c, _, _ := relayMediaAdapter(t, relayMediaDesc("send", "edit", "typing", "get_chat_info"))
	r, err := a.SendImage(context.Background(), "chat1", "https://x.io/a.png", "hi", "", nil)
	ops := relayOpsOf(c)
	action, _ := c.last(t)
	if err != nil || !r.Success || slices.Contains(ops, "send_media") || ops[len(ops)-1] != "send" || !strings.Contains(action["content"].(string), "https://x.io/a.png") {
		t.Fatalf("result %+v err %v ops %v", r, err, ops)
	}
}

func TestLegacyConnectorLocalFileKeepsItsFallbackReceipt(t *testing.T) {
	a, c, uploads, _ := relayMediaAdapter(t, relayMediaDesc("send", "edit", "typing", "get_chat_info"))
	r, err := a.SendDocument(context.Background(), "chat1", relayTempFile(t, "report.pdf", []byte("%PDF")), "", "", "", nil, nil)
	if err != nil || !r.Success || len(uploads()) != 0 || !slices.Equal(relayOpsOf(c), []any{"send"}) {
		t.Fatalf("result %+v err %v ops %v", r, err, relayOpsOf(c))
	}
}

func TestConnectorDeclineAfterUploadIsAFailedSendWithOneNotice(t *testing.T) {
	a, c, uploads, _ := relayMediaAdapter(t, relayMediaDesc())
	c.result = func(action map[string]any) (map[string]any, error) {
		if action["op"] == "send_media" {
			return map[string]any{"success": false, "error": "file too large for the platform"}, nil
		}
		return relayConnectorResult(action)
	}
	r, err := a.SendDocument(context.Background(), "chat1", relayTempFile(t, "report.pdf", []byte("%PDF")), "Q3", "", "", nil, nil)
	raw, _ := r.RawResponse.(map[string]any)
	last, _ := c.last(t)
	if err != nil || r.Success || len(uploads()) != 1 || !strings.Contains(relayErrText(r), "connector did not deliver") || raw["failure_notice_sent"] != true ||
		!slices.Equal(relayOpsOf(c), []any{"send_media", "send"}) || !strings.Contains(last["content"].(string), "Q3") {
		t.Fatalf("result %+v err %v ops %v", r, err, relayOpsOf(c))
	}
}

func TestLostAckOnSendMediaIsNotReportedAsAnUndeliveredAttachment(t *testing.T) {
	a, c, _, _ := relayMediaAdapter(t, relayMediaDesc())
	c.result = func(action map[string]any) (map[string]any, error) {
		if action["op"] == "send_media" {
			return map[string]any{"success": false, "error": "relay outbound timed out", "ambiguous": true}, nil
		}
		return relayConnectorResult(action)
	}
	r, err := a.SendDocument(context.Background(), "chat1", relayTempFile(t, "report.pdf", []byte("%PDF")), "Q3", "", "", nil, nil)
	raw, _ := r.RawResponse.(map[string]any)
	if err != nil || !r.Success || raw["ambiguous"] != true || !slices.Equal(relayOpsOf(c), []any{"send_media"}) {
		t.Fatalf("result %+v err %v ops %v", r, err, relayOpsOf(c))
	}
}

func TestInboundWithoutClientKeepsPublicDropsRehost(t *testing.T) {
	a, _ := relayConnectorAdapter(t, relayMediaDesc())
	e := relayInbound(Platform("telegram"), "c1", "dm", func(s *SessionSource) { s.UserID = relayStrPtr("u1") })
	e.MediaURLs = []string{"https://conn.example/relay/media/deadbeef", "https://cdn.discordapp.com/attachments/a/b.png"}
	if err := a.localizeInboundMedia(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.MediaURLs, []string{"https://cdn.discordapp.com/attachments/a/b.png"}) {
		t.Fatalf("media %v", e.MediaURLs)
	}
}

// Printed form of an action for a substring check
func relayReprOf(action map[string]any) string {
	var b strings.Builder
	for k, v := range action {
		b.WriteString(k + "=" + strings.ReplaceAll(strings.TrimSpace(relayWireStr(v)), "\n", " ") + ";")
	}
	return b.String()
}
