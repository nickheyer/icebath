package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Text block
func textBlock(s string) *ContentBlock { return &ContentBlock{Type: "text", Text: s} }

// Rendered handler JSON decoded into a map
func render(t *testing.T, env *tools.Env, r *CallToolResult) map[string]any {
	t.Helper()
	var out map[string]any
	raw := RenderCallToolResult(env, r, "test-server", nil)
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("render %q: %v", raw, err)
	}
	return out
}

// PNG bytes of a solid square
func pngBytes(t *testing.T, side int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	img.Set(0, 0, color.RGBA{1, 2, 3, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestStructuredContentArbitration(t *testing.T) {
	captureLogs(t)
	if got := render(t, nil, &CallToolResult{Content: []*ContentBlock{textBlock("hello")}}); !reflect.DeepEqual(got, map[string]any{"result": "hello"}) {
		t.Fatalf("text only %v", got)
	}
	payload := map[string]any{"value": "x", "n": 1.0}
	if got := render(t, nil, &CallToolResult{Content: []*ContentBlock{}, StructuredContent: payload}); !reflect.DeepEqual(got["result"], payload) {
		t.Fatalf("structured only %v", got)
	}
	dual, _ := json.Marshal(payload)
	if got := render(t, nil, &CallToolResult{Content: []*ContentBlock{textBlock(string(dual))}, StructuredContent: payload}); !reflect.DeepEqual(got, map[string]any{"result": string(dual)}) {
		t.Fatalf("dual emit %v", got)
	}
	accent := map[string]any{"a": "h\u00e9llo", "b": 2.0}
	indented := "{\n  \"b\": 2,\n  \"a\": \"h\\u00e9llo\"\n}"
	got := render(t, nil, &CallToolResult{Content: []*ContentBlock{textBlock(indented), textBlock("done")}, StructuredContent: accent})
	if len(got) != 1 || strings.Count(got["result"].(string), `"b"`) != 1 || !strings.HasSuffix(got["result"].(string), "done") {
		t.Fatalf("reordered dual emit %v", got)
	}
	big := map[string]any{"k": strings.Repeat("z", hardResultCapChars+10)}
	if got := render(t, nil, &CallToolResult{Content: []*ContentBlock{textBlock("done")}, StructuredContent: big}); !reflect.DeepEqual(got, map[string]any{"result": "done"}) {
		t.Fatalf("over cap stand in kept beside text %v", got)
	}
	structured := map[string]any{"data": []any{map[string]any{"jid": "12345@s.whatsapp.net"}}}
	got = render(t, nil, &CallToolResult{Content: []*ContentBlock{textBlock("Retrieved 1 chats (offset 0, limit 1)")}, StructuredContent: structured})
	if got["result"] != "Retrieved 1 chats (offset 0, limit 1)" || !reflect.DeepEqual(got["structuredContent"], structured) {
		t.Fatalf("summary plus data %v", got)
	}
	if raw := RenderCallToolResult(nil, &CallToolResult{Content: []*ContentBlock{textBlock("just text")}}, "s", nil); raw != `{"result":"just text"}` {
		t.Fatalf("text only wire %s", raw)
	}
	status := map[string]any{"status": "ok"}
	if got := render(t, nil, &CallToolResult{Content: []*ContentBlock{textBlock("   \n")}, StructuredContent: status}); !reflect.DeepEqual(got["structuredContent"], status) {
		t.Fatalf("whitespace text %v", got)
	}
}

func TestMetaSurfacing(t *testing.T) {
	cases := []struct {
		meta map[string]any
		sc   any
		want map[string]any
	}{
		{map[string]any{"com.example/handoff": map[string]any{"url": "https://x"}}, nil,
			map[string]any{"result": "done", "_meta": map[string]any{"com.example/handoff": map[string]any{"url": "https://x"}}}},
		{map[string]any{"modelcontextprotocol.io/x": 1.0, "tools.mcp.com/y": 2.0, "com.example.mcp/z": 3.0, "plain": 4.0}, nil,
			map[string]any{"result": "done", "_meta": map[string]any{"com.example.mcp/z": 3.0, "plain": 4.0}}},
		{map[string]any{"mcp.io/internal": true}, nil, map[string]any{"result": "done"}},
		{map[string]any{"com.example/k": "v"}, map[string]any{"ok": true},
			map[string]any{"result": "done", "structuredContent": map[string]any{"ok": true}, "_meta": map[string]any{"com.example/k": "v"}}},
	}
	for _, c := range cases {
		if got := render(t, nil, &CallToolResult{Content: []*ContentBlock{textBlock("done")}, StructuredContent: c.sc, Meta: c.meta}); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("meta %v got %v", c.meta, got)
		}
	}
	raw := RenderCallToolResult(nil, &CallToolResult{Content: []*ContentBlock{textBlock("done")}, StructuredContent: map[string]any{"ok": true}, Meta: map[string]any{"com.example/k": "v"}}, "s", nil)
	if raw != `{"result":"done","structuredContent":{"ok":true},"_meta":{"com.example/k":"v"}}` {
		t.Fatalf("key order %s", raw)
	}
	if raw := RenderCallToolResult(nil, &CallToolResult{Meta: map[string]any{"com.example/k": "v"}}, "s", nil); raw != `{"_meta":{"com.example/k":"v"},"result":""}` {
		t.Fatalf("empty text order %s", raw)
	}
	for key, want := range map[string]bool{"modelcontextprotocol.io/x": true, "mcp.dev/x": true, "tools.mcp.com/x": true,
		"com.example.mcp/x": false, "com.example/x": false, "plain-key": false, "/leading-slash": false} {
		if isReservedMetaKey(key) != want {
			t.Fatalf("%s reserved want %v", key, want)
		}
	}
}

func TestDroppedBlockNoticeAndErrors(t *testing.T) {
	captureLogs(t)
	size := int64(1234)
	weird := &ContentBlock{Type: "hologram", MimeType: "application/x-hologram", Size: &size}
	got := render(t, nil, &CallToolResult{Content: []*ContentBlock{weird}})
	for _, want := range []string{"[MCP content dropped: unsupported block", "type=hologram", "mimeType=application/x-hologram", "size=1234"} {
		if !strings.Contains(got["result"].(string), want) {
			t.Fatalf("notice %v lacks %s", got, want)
		}
	}
	payload := map[string]any{"k": "v"}
	got = render(t, nil, &CallToolResult{Content: []*ContentBlock{weird}, StructuredContent: payload})
	if !reflect.DeepEqual(got["structuredContent"], payload) || !strings.Contains(got["result"].(string), "[MCP content dropped") {
		t.Fatalf("drop notice suppressed structured %v", got)
	}
	errText := "boom token=abc123 "
	resText := "resource failure"
	got = render(t, nil, &CallToolResult{IsError: true, Content: []*ContentBlock{textBlock(errText), {Type: "resource", Resource: &ResourceContents{URI: "x", Text: &resText}}}})
	if got["error"] != "boom [REDACTED] resource failure" {
		t.Fatalf("error %v", got)
	}
	if got := render(t, nil, &CallToolResult{IsError: true}); got["error"] != "MCP tool returned an error" {
		t.Fatalf("empty error %v", got)
	}
}

func TestResultSizeLimit(t *testing.T) {
	short := "hello"
	if truncateTextResult(short) != short || truncateTextResult(strings.Repeat("a", hardResultCapChars)) != strings.Repeat("a", hardResultCapChars) {
		t.Fatal("results within the cap must pass unchanged")
	}
	spill := strings.Repeat("x", 100000)
	if truncateTextResult(spill) != spill {
		t.Fatal("spillover sized result truncated")
	}
	huge := "HEAD" + strings.Repeat("m", hardResultCapChars*2) + "TAIL"
	out := truncateTextResult(huge)
	if len([]rune(out)) >= len([]rune(huge)) || !strings.HasPrefix(out, "HEAD") || !strings.HasSuffix(out, "TAIL") || !strings.Contains(out, "MCP RESULT") {
		t.Fatalf("truncation %d", len(out))
	}
	if hardResultCapChars <= 50000 {
		t.Fatal("hard cap must sit above the spillover threshold")
	}
}

func TestImageBlocksCachedAsMedia(t *testing.T) {
	h := isolateHome(t)
	captureLogs(t)
	env := &tools.Env{Home: h}
	if imageExtensionForMime("image/jpeg") != ".jpg" || imageExtensionForMime("image/jpg") != ".jpg" || imageExtensionForMime("image/x-unknown") != ".png" || imageExtensionForMime("IMAGE/PNG; q=1") != ".png" {
		t.Fatal("image extensions")
	}
	data := pngBytes(t, 4)
	tag := cacheImageBlock(env, &ContentBlock{Type: "image", Data: base64.StdEncoding.EncodeToString(data), MimeType: "image/png"})
	if !strings.HasPrefix(tag, "MEDIA:") || !strings.HasSuffix(tag, ".png") {
		t.Fatalf("tag %q", tag)
	}
	if got, _ := os.ReadFile(strings.TrimPrefix(tag, "MEDIA:")); !bytes.Equal(got, data) {
		t.Fatal("cached bytes differ")
	}
	jpeg := cacheImageBlock(env, &ContentBlock{Type: "image", Data: base64.StdEncoding.EncodeToString([]byte("\xff\xd8\xff\xe0rest")), MimeType: "image/jpeg"})
	if !strings.HasSuffix(jpeg, ".jpg") {
		t.Fatalf("jpeg %q", jpeg)
	}
	for _, blk := range []*ContentBlock{
		{Type: "text", Text: "x"},
		{Type: "image", MimeType: "image/png"},
		{Type: "image", Data: base64.StdEncoding.EncodeToString([]byte("<html>nope</html>")), MimeType: "image/png"},
		{Type: "image", Data: "!!!not base64", MimeType: "image/png"},
	} {
		if got := cacheImageBlock(env, blk); got != "" {
			t.Fatalf("%+v cached as %q", blk, got)
		}
	}
	var paths []string
	raw := RenderCallToolResult(env, &CallToolResult{Content: []*ContentBlock{{Type: "image", Data: base64.StdEncoding.EncodeToString(data), MimeType: "image/png"}, textBlock("caption")}}, "srv", &paths)
	if len(paths) != 1 || !strings.Contains(raw, "MEDIA:"+paths[0]) {
		t.Fatalf("render paths %v %s", paths, raw)
	}
}

func TestNativeImageEnvelope(t *testing.T) {
	h := isolateHome(t)
	captureLogs(t)
	p := filepath.Join(h, "shot.png")
	os.WriteFile(p, pngBytes(t, 64), 0o600)
	textEnv := &tools.Env{Home: h, Config: config.Tree{"agent": map[string]any{"image_input_mode": "text"}}}
	if got := resultWithNativeImages(context.Background(), textEnv, "s1", `{"result": "x"}`, []string{p}); got != `{"result": "x"}` {
		t.Fatalf("text mode must keep the string %v", got)
	}
	if got := resultWithNativeImages(context.Background(), textEnv, "s1", "plain", nil); got != "plain" {
		t.Fatalf("no images %v", got)
	}
	part := nativeImagePart(textEnv, p)
	if part == nil || !strings.HasPrefix(part.part["image_url"].(map[string]any)["url"].(string), "data:image/") {
		t.Fatalf("native part %v", part)
	}
	broken := filepath.Join(h, "broken.png")
	os.WriteFile(broken, pngBytes(t, 64)[:60], 0o600)
	if nativeImagePart(textEnv, broken) != nil {
		t.Fatal("undecodable image prepared")
	}
}

func TestResourceBlocks(t *testing.T) {
	h := isolateHome(t)
	captureLogs(t)
	env := &tools.Env{Home: h}
	pdf := []byte("%PDF-1.4 tiny")
	blob := base64.StdEncoding.EncodeToString(pdf)
	out := renderResourceBlock(env, &ContentBlock{Type: "resource", Resource: &ResourceContents{URI: "file:///docs/report.pdf", MimeType: "application/pdf", Blob: &blob}}, "srv")
	if !strings.HasPrefix(out, "[MCP resource saved to ") || !strings.Contains(out, "(application/pdf, 13 bytes)") || !strings.Contains(out, "report.pdf") {
		t.Fatalf("pdf %q", out)
	}
	bad := "!!!not-base64!!!"
	if out := renderResourceBlock(env, &ContentBlock{Type: "resource", Resource: &ResourceContents{URI: "u", MimeType: "application/pdf", Blob: &bad}}, "srv"); out != "[MCP embedded resource could not be decoded: application/pdf]" {
		t.Fatalf("malformed %q", out)
	}
	if renderResourceBlock(env, textBlock("x"), "srv") != "" || renderResourceBlock(env, &ContentBlock{Type: "image"}, "srv") != "" {
		t.Fatal("non resource blocks must render empty")
	}
	if got := resourceFilename("file:///a/b/..%2F..%2Fetc%2Fpasswd", ""); strings.Contains(got, "/") {
		t.Fatalf("traversal %q", got)
	}
	if resourceFilename("https://x/files/report%20final.pdf?x=1", "") != "report final.pdf" || resourceFilename("", "application/pdf") != "resource.pdf" || resourceFilename("x:", "zz/zz") != "resource.bin" {
		t.Fatal("filename hints")
	}
	long := resourceFilename("file:///"+strings.Repeat("n", 200)+".json", "")
	if len(long) != 150 || !strings.HasSuffix(long, ".json") {
		t.Fatalf("long %d %q", len(long), long[len(long)-8:])
	}
	huge := strings.Repeat("A", resourceMaxB64Chars+4)
	if out := renderResourceBlock(env, &ContentBlock{Type: "resource", Resource: &ResourceContents{URI: "u", Blob: &huge}}, "srv"); !strings.HasPrefix(out, "[MCP embedded resource too large to cache: ~") || !strings.HasSuffix(out, ", uri=u]") {
		t.Fatalf("oversized %q", out)
	}
	link := renderResourceBlock(env, &ContentBlock{Type: "resource_link", URI: "res://a", Name: "a", MimeType: "text/plain"}, "srv")
	if link != "[MCP resource link: uri=res://a, name=a, mimeType=text/plain \u2014 fetch it with mcp__srv__read_resource]" {
		t.Fatalf("link %q", link)
	}
	text := "\U000e0041embedded"
	if out := renderResourceBlock(env, &ContentBlock{Type: "resource", Resource: &ResourceContents{URI: "u", Text: &text}}, "srv"); out != "embedded" {
		t.Fatalf("embedded text %q", out)
	}
	audio := cacheAudioBlock(env, &ContentBlock{Type: "audio", Data: base64.StdEncoding.EncodeToString([]byte("RIFFxxxxWAVE")), MimeType: "audio/x-wav"})
	if !strings.HasPrefix(audio, "MEDIA:") || !strings.HasSuffix(audio, ".wav") {
		t.Fatalf("audio %q", audio)
	}
	if cacheAudioBlock(env, &ContentBlock{Type: "image", Data: "AAAA", MimeType: "image/png"}) != "" {
		t.Fatal("non audio cached as audio")
	}
}

func TestNativeImageEnvelopeOnAVisionRoute(t *testing.T) {
	h := isolateHome(t)
	captureLogs(t)
	p := filepath.Join(h, "shot.png")
	os.WriteFile(p, pngBytes(t, 64), 0o600)
	env := &tools.Env{Home: h, Values: &sync.Map{}, Config: config.Tree{"agent": map[string]any{"image_input_mode": "native"},
		"model": map[string]any{"provider": "anthropic", "default": "claude-sonnet-4-5"}}}
	got, ok := resultWithNativeImages(context.Background(), env, "s1", `{"result":"MEDIA:`+p+`"}`, []string{p}).(map[string]any)
	if !ok || got["_multimodal"] != true || got["text_summary"] != `{"result":"MEDIA:`+p+`"}` {
		t.Fatalf("envelope %v", got)
	}
	content := got["content"].([]any)
	if len(content) != 3 || content[2].(map[string]any)["type"] != "image_url" ||
		!strings.HasPrefix(content[1].(map[string]any)["text"].(string), "The image(s) from this call are attached") {
		t.Fatalf("content %v", content)
	}
}
