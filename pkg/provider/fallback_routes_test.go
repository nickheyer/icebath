package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func fallbackResolver(t *testing.T, dotenv string) *resolver {
	t.Helper()
	h := t.TempDir()
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte(dotenv), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return newResolver(context.Background(), config.Tree{}, config.NewSecrets(h), h, nil)
}

func TestResolveEntryAPIKey(t *testing.T) {
	t.Setenv("FB_KEY", "env-key")
	r := fallbackResolver(t, "")
	if got := r.ResolveEntryAPIKey(map[string]any{"provider": "custom", "api_key": "inline-key", "key_env": "FB_KEY"}); got != "inline-key" {
		t.Fatalf("inline wins: %q", got)
	}
	if got := r.ResolveEntryAPIKey(map[string]any{"provider": "openrouter", "model": "glm"}); got != "" {
		t.Fatalf("no key fields: %q", got)
	}
	if got := r.ResolveEntryAPIKey(map[string]any{"api_key": "   ", "key_env": "FB_KEY"}); got != "env-key" {
		t.Fatalf("whitespace inline falls through: %q", got)
	}
	scoped := fallbackResolver(t, "FB_KEY=fake-active-profile-key\n")
	if got := scoped.ResolveEntryAPIKey(map[string]any{"key_env": "FB_KEY"}); got != "fake-active-profile-key" {
		t.Fatalf("profile scope wins over process env: %q", got)
	}
	if got := r.ResolveEntryAPIKey(nil); got != "" {
		t.Fatalf("nil entry: %q", got)
	}
}

func TestEffectiveRuntimeProvider(t *testing.T) {
	entry := map[string]any{"provider": "my-custom-provider", "model": "some-model"}
	if got := EffectiveRuntimeProvider(entry, &Runtime{Provider: "custom", RequestedProvider: "my-custom-provider"}); got != "my-custom-provider" {
		t.Fatalf("got %q", got)
	}
	if got := EffectiveRuntimeProvider(entry, &Runtime{Provider: "custom"}); got != "my-custom-provider" {
		t.Fatalf("entry fallback: %q", got)
	}
	if got := EffectiveRuntimeProvider(map[string]any{"provider": "openrouter", "model": "glm"}, &Runtime{Provider: "openrouter", RequestedProvider: "openrouter"}); got != "openrouter" {
		t.Fatalf("builtin: %q", got)
	}
	if got := EffectiveRuntimeProvider(map[string]any{"provider": "custom", "model": "m"}, &Runtime{Provider: "custom", RequestedProvider: "custom"}); got != "custom" {
		t.Fatalf("bare custom: %q", got)
	}
	if got := EffectiveRuntimeProvider(nil, nil); got != "" {
		t.Fatalf("nil inputs: %q", got)
	}
}

func TestFallbackChainDedupsCanonicalIDAndAlias(t *testing.T) {
	cfg := map[string]any{"fallback_providers": []any{
		map[string]any{"provider": "opencode", "model": "deepseek-v4-flash-free"},
		map[string]any{"provider": "opencode-zen", "model": "deepseek-v4-flash-free"},
		map[string]any{"provider": "anthropic", "model": "claude-sonnet-4"},
	}}
	var providers []string
	for _, e := range GetFallbackChain(cfg) {
		providers = append(providers, cfgString(e["provider"]))
	}
	if !reflect.DeepEqual(providers, []string{"opencode", "anthropic"}) {
		t.Fatalf("got %v", providers)
	}
}

func TestFallbackChainMergesLegacyAndNormalizesBaseURL(t *testing.T) {
	cfg := map[string]any{
		"fallback_providers": []any{map[string]any{"provider": "a", "model": "m", "base_url": " https://x/v1/ "}, map[string]any{"provider": "", "model": "m"}, "junk"},
		"fallback_model":     map[string]any{"provider": "A", "model": "M", "base_url": "https://x/v1"},
	}
	chain := GetFallbackChain(cfg)
	if len(chain) != 1 || chain[0]["base_url"] != "https://x/v1" {
		t.Fatalf("got %v", chain)
	}
	if n := PreAgentFallbackNotice("a", "m", "", ""); n != "⚠️ Provider fallback: a/m unavailable; using fallback for this response." {
		t.Fatalf("notice: %q", n)
	}
}

func TestScopedFallbackChain(t *testing.T) {
	inherited := []map[string]any{{"provider": "a", "model": "m"}}
	if got := ScopedFallbackChain(inherited, nil, false, false, "cron"); !reflect.DeepEqual(got, inherited) {
		t.Fatalf("unpinned inherits: %v", got)
	}
	if got := ScopedFallbackChain(inherited, nil, false, true, "cron"); got != nil {
		t.Fatalf("pinned never inherits: %v", got)
	}
	if got := ScopedFallbackChain(inherited, []any{}, true, false, "cron"); got != nil {
		t.Fatalf("explicit empty disables: %v", got)
	}
	if got := ScopedFallbackChain(inherited, []any{map[string]any{"provider": "b", "model": "n"}}, true, true, "cron"); len(got) != 1 || got[0]["provider"] != "b" {
		t.Fatalf("own chain: %v", got)
	}
	if got := ScopedFallbackChain(inherited, []any{"junk"}, true, false, "cron"); !reflect.DeepEqual(got, inherited) {
		t.Fatalf("unusable declared falls to default: %v", got)
	}
}

func TestResolveFallbackEntryAPIKeyExport(t *testing.T) {
	h := runtimeConfigHome(t, "model:\n  default: m\n")
	t.Setenv("FALLBACK_ENTRY_KEY", "  sk-from-env  ")
	ctx := context.Background()
	if got := ResolveFallbackEntryAPIKey(ctx, h, map[string]any{"api_key": " sk-inline ", "key_env": "FALLBACK_ENTRY_KEY"}); got != "sk-inline" {
		t.Fatalf("inline %q", got)
	}
	if got := ResolveFallbackEntryAPIKey(ctx, h, map[string]any{"api_key_env": "FALLBACK_ENTRY_KEY"}); got != "sk-from-env" {
		t.Fatalf("key env alias %q", got)
	}
	if got := ResolveFallbackEntryAPIKey(ctx, h, nil); got != "" {
		t.Fatalf("nil entry %q", got)
	}
}
