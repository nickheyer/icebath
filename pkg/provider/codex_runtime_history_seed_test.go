package provider

import (
	"strings"
	"testing"
)

func TestRenderHistorySeedRowsAndCaps(t *testing.T) {
	if RenderHistorySeed(nil) != "" || RenderHistorySeed([]map[string]any{{"role": "user", "content": "only the current turn"}}) != "" {
		t.Fatal("nothing before the current user message must render empty")
	}
	got := RenderHistorySeed([]map[string]any{
		{"role": "system", "content": "prompt"},
		{"role": "user", "content": []any{"look", map[string]any{"type": "text", "text": "here"}, map[string]any{"type": "image_url"}}},
		{"role": "assistant", "content": "  ok  ", "tool_calls": []any{map[string]any{"function": map[string]any{"name": "web_search"}}, map[string]any{"function": map[string]any{}}}},
		{"role": "tool", "content": strings.Repeat("x", 450)},
		{"role": "user", "content": "current"},
	})
	want := codexHistorySeedHeader + "\n\n[USER]\nlook\nhere\n\n[ASSISTANT]\nok\n[ASSISTANT called tools: web_search]\n\n[TOOL RESULT]\n" + strings.Repeat("x", 400) + " …"
	if got != want {
		t.Fatalf("seed %q", got)
	}
	long := RenderHistorySeed([]map[string]any{{"role": "user", "content": strings.Repeat("é", MaxHistorySeedChars)}, {"role": "assistant", "content": "tail"}})
	if !strings.HasPrefix(long, codexHistorySeedHeader+"\n\n[… earlier turns omitted …]\n\n") || !strings.HasSuffix(long, "[ASSISTANT]\ntail") {
		t.Fatalf("cap prefix or tail wrong")
	}
	if body := strings.TrimPrefix(long, codexHistorySeedHeader+"\n\n[… earlier turns omitted …]\n\n"); len([]rune(body)) != MaxHistorySeedChars {
		t.Fatalf("kept %d code points", len([]rune(body)))
	}
}
