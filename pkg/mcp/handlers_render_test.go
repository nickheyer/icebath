package mcp

import (
	"context"
	"encoding/base64"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Decoded handler output for one wire result served by a fake server
func handlerRender(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	handlerServer(t, "test-server", func(string, map[string]any) (any, error) { return result, nil })
	out := makeToolHandler("test-server", "my-tool", 30, false)(context.Background(), &tools.Call{Name: "mcp_test_server_my_tool", Env: &tools.Env{}})
	return decodeResult(t, out)
}

func wireText(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func TestHandlerRendersStructuredContentAndMetaFromTheWire(t *testing.T) {
	structured := map[string]any{"data": []any{map[string]any{"jid": "12345@s.whatsapp.net"}}}
	cases := []struct {
		name   string
		result map[string]any
		want   map[string]any
		check  func(map[string]any) bool
	}{
		{"text only", map[string]any{"content": []any{wireText("hello")}}, map[string]any{"result": "hello"}, nil},
		{"structured null", map[string]any{"content": []any{wireText("done")}, "structuredContent": nil}, map[string]any{"result": "done"}, nil},
		{"empty text with structured", map[string]any{"content": []any{}, "structuredContent": map[string]any{"k": "v"}},
			map[string]any{"result": map[string]any{"k": "v"}}, nil},
		{"vendor meta", map[string]any{"content": []any{wireText("done")}, "_meta": map[string]any{"com.example/handoff": map[string]any{"url": "https://x"}}},
			map[string]any{"result": "done", "_meta": map[string]any{"com.example/handoff": map[string]any{"url": "https://x"}}}, nil},
		{"reserved meta dropped", map[string]any{"content": []any{wireText("done")}, "_meta": map[string]any{"modelcontextprotocol.io/progress": 1,
			"tools.mcp.com/trace": "x", "com.example.mcp/vendor": "keep", "unprefixed": "keep"}},
			map[string]any{"result": "done", "_meta": map[string]any{"com.example.mcp/vendor": "keep", "unprefixed": "keep"}}, nil},
		{"all reserved meta", map[string]any{"content": []any{wireText("done")}, "_meta": map[string]any{"mcp.io/internal": true}}, map[string]any{"result": "done"}, nil},
		{"meta with structured", map[string]any{"content": []any{wireText("txt")}, "structuredContent": map[string]any{"ok": true}, "_meta": map[string]any{"com.example/k": "v"}},
			map[string]any{"result": "txt", "structuredContent": map[string]any{"ok": true}, "_meta": map[string]any{"com.example/k": "v"}}, nil},
		{"dual emit", map[string]any{"content": []any{wireText(`{"status": "ok", "count": 2}`)}, "structuredContent": map[string]any{"status": "ok", "count": 2}},
			map[string]any{"result": `{"status": "ok", "count": 2}`}, nil},
		{"summary plus data", map[string]any{"content": []any{wireText("Retrieved 1 chats (offset 0, limit 1)")}, "structuredContent": structured},
			map[string]any{"result": "Retrieved 1 chats (offset 0, limit 1)", "structuredContent": structured}, nil},
		{"whitespace only content", map[string]any{"content": []any{wireText("   \n")}, "structuredContent": map[string]any{"status": "ok"}}, nil,
			func(d map[string]any) bool {
				return reflect.DeepEqual(d["structuredContent"], map[string]any{"status": "ok"})
			}},
		{"structured only", map[string]any{"content": []any{}, "structuredContent": map[string]any{"only": "structured"}},
			map[string]any{"result": map[string]any{"only": "structured"}}, nil},
		{"unsupported block", map[string]any{"content": []any{map[string]any{"type": "hologram", "mimeType": "application/x-hologram", "size": 1234}}}, nil,
			func(d map[string]any) bool {
				s, _ := d["result"].(string)
				return strings.Contains(s, "[MCP content dropped: unsupported block") && strings.Contains(s, "type=hologram") &&
					strings.Contains(s, "mimeType=application/x-hologram") && strings.Contains(s, "size=1234")
			}},
		{"drop notice keeps structured", map[string]any{"content": []any{map[string]any{"type": "hologram"}}, "structuredContent": map[string]any{"real": "data"}}, nil,
			func(d map[string]any) bool {
				s, _ := d["result"].(string)
				return reflect.DeepEqual(d["structuredContent"], map[string]any{"real": "data"}) && strings.Contains(s, "[MCP content dropped")
			}},
		{"error resource text", map[string]any{"isError": true, "content": []any{map[string]any{"type": "resource", "resource": map[string]any{
			"uri": "mem://err", "mimeType": "text/plain", "text": "quota exceeded for workspace W1"}}}}, nil,
			func(d map[string]any) bool {
				s, _ := d["error"].(string)
				return strings.Contains(s, "quota exceeded for workspace W1")
			}},
		{"error mixed text and resource", map[string]any{"isError": true, "content": []any{wireText("tool failed"), map[string]any{"type": "resource",
			"resource": map[string]any{"uri": "mem://err", "mimeType": "text/plain", "text": " — details in resource"}}}}, nil,
			func(d map[string]any) bool {
				s, _ := d["error"].(string)
				return strings.Contains(s, "tool failed") && strings.Contains(s, "details in resource")
			}},
		{"error without text", map[string]any{"isError": true, "content": []any{}}, nil,
			func(d map[string]any) bool { s, _ := d["error"].(string); return s != "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := handlerRender(t, tc.result)
			if tc.check != nil {
				if !tc.check(got) {
					t.Fatalf("got %v", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}
}

func TestHandlerTruncatesAPathologicalTextResult(t *testing.T) {
	huge := "HEAD" + strings.Repeat("m", hardResultCapChars*2) + "TAIL"
	got, _ := handlerRender(t, map[string]any{"content": []any{wireText(huge)}})["result"].(string)
	if got != truncateTextResult(huge) || len(got) >= len(huge) || !strings.Contains(got, "MCP RESULT") {
		t.Fatalf("handler result of %d chars", len(got))
	}
	spill := strings.Repeat("x", 100000)
	if got := handlerRender(t, map[string]any{"content": []any{wireText(spill)}})["result"]; got != spill {
		t.Fatal("spillover sized result truncated by the handler")
	}
}

func TestHandlerReturnsTheNativeImageEnvelope(t *testing.T) {
	h := isolateHome(t)
	captureLogs(t)
	png := base64.StdEncoding.EncodeToString(pngBytes(t, 64))
	handlerServer(t, "srv", func(string, map[string]any) (any, error) {
		return map[string]any{"content": []any{map[string]any{"type": "image", "data": png, "mimeType": "image/png"}}}, nil
	})
	env := &tools.Env{Home: h, Values: &sync.Map{}, Config: config.Tree{"agent": map[string]any{"image_input_mode": "native"},
		"model": map[string]any{"provider": "anthropic", "default": "claude-sonnet-4-5"}}}
	out := decodeResult(t, makeToolHandler("srv", "shot", 30, true)(context.Background(), &tools.Call{Name: "mcp__srv__shot", Env: env, SessionID: "s1"}))
	content, _ := out["content"].([]any)
	if out["_multimodal"] != true || len(content) != 3 || content[2].(map[string]any)["type"] != "image_url" {
		t.Fatalf("envelope %v", out)
	}
	summary, _ := out["text_summary"].(string)
	media := strings.TrimSuffix(strings.SplitN(summary, "MEDIA:", 2)[1], `"}`)
	if _, err := os.Stat(media); err != nil {
		t.Fatalf("cached image %q from %q: %v", media, summary, err)
	}
	plain := decodeResult(t, makeToolHandler("srv", "shot", 30, false)(context.Background(), &tools.Call{Name: "mcp__srv__shot", Env: env, SessionID: "s1"}))
	if _, multimodal := plain["_multimodal"]; multimodal {
		t.Fatalf("native images off still built an envelope %v", plain)
	}
}
