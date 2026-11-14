package provider

import (
	"testing"
)

func TestLoadRuntimePoolWithReadsResolvedKeyThroughLoad(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	env.Config = func() map[string]any {
		return map[string]any{"providers": map[string]any{"gemini-no-filter": map[string]any{
			"name": "Gemini Display", "api": "https://generativelanguage.googleapis.com/v1beta"}}}
	}
	var keys []string
	want := NewPool(env, "openrouter", nil)
	load := func(_ PoolEnv, key string) *Pool { keys = append(keys, key); return want }
	if got := LoadRuntimePoolWith(env, load, "openrouter", "https://openrouter.ai/api/v1", "", nil); got != want {
		t.Fatalf("built-in pool %v", got)
	}
	owner := map[string]any{"name": "Gemini Display", "provider_key": "gemini-no-filter", "api": "https://generativelanguage.googleapis.com/v1beta"}
	snapshot := map[string]any{SnapshotOwnerKey: owner}
	if got := LoadRuntimePoolWith(env, load, "custom", "https://generativelanguage.googleapis.com/v1beta", "custom:gemini-no-filter", snapshot); got != want {
		t.Fatalf("custom pool %v", got)
	}
	if got := LoadRuntimePoolWith(env, load, "custom", "https://x.example/v1", "custom:gemini-no-filter", map[string]any{}); got != nil {
		t.Fatalf("named identity without owner loaded %v", got)
	}
	if len(keys) != 2 || keys[0] != "openrouter" || keys[1] != "gemini-no-filter" {
		t.Fatalf("keys %v", keys)
	}
}
