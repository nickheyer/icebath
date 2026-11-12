package agent

import (
	"context"
	"testing"
)

// Every cache_control marker reachable in a value
func moaCacheControls(v any) []map[string]any {
	var out []map[string]any
	switch t := v.(type) {
	case map[string]any:
		if m, ok := t["cache_control"].(map[string]any); ok {
			out = append(out, m)
		}
		for _, x := range t {
			out = append(out, moaCacheControls(x)...)
		}
	case []map[string]any:
		for _, x := range t {
			out = append(out, moaCacheControls(x)...)
		}
	case []any:
		for _, x := range t {
			out = append(out, moaCacheControls(x)...)
		}
	}
	return out
}

func TestMoACacheControlThreadsTtl(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	off := false
	anthropic := map[string]any{"provider": "anthropic", "model": "claude-opus-4.8", "base_url": "", "api_mode": "anthropic_messages"}
	t.Run("test_moa_decoration_uses_threaded_1h", func(t *testing.T) {
		messages := []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "q1"}, {"role": "assistant", "content": "a1"}, {"role": "user", "content": "q2"}}
		out := maybeApplyMoaCacheControl(ctx, messages, anthropic, &off, "1h")
		markers := moaCacheControls(out)
		if len(markers) == 0 {
			t.Fatal("expected MoA decoration on a caching route")
		}
		for _, m := range markers {
			if m["ttl"] != "1h" {
				t.Fatalf("markers %v", markers)
			}
		}
		if len(moaCacheControls(messages)) != 0 {
			t.Fatal("caller messages decorated")
		}
	})
	t.Run("test_moa_qwen_1h_clamped_to_5m", func(t *testing.T) {
		messages := []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "q1"}}
		out := maybeApplyMoaCacheControl(ctx, messages, map[string]any{"provider": "opencode", "model": "qwen3.6-plus", "base_url": "", "api_mode": "chat_completions"}, &off, "1h")
		markers := moaCacheControls(out)
		if len(markers) == 0 {
			t.Fatal("opencode+qwen is a cache-honoring MoA route")
		}
		for _, m := range markers {
			if _, has := m["ttl"]; has {
				t.Fatalf("markers %v", markers)
			}
		}
	})
	t.Run("test_moa_decoration_defaults_to_5m_without_ttl", func(t *testing.T) {
		messages := []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "q1"}}
		markers := moaCacheControls(maybeApplyMoaCacheControl(ctx, messages, anthropic, &off, ""))
		if len(markers) == 0 {
			t.Fatal("no markers")
		}
		for _, m := range markers {
			if _, has := m["ttl"]; has {
				t.Fatalf("markers %v", markers)
			}
		}
	})
}
