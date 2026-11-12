package agent

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestContextRouteMismatchNamedCustomProvider(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	check := func(t *testing.T, cfgURL, activeURL, cfgProv, activeProv string, want bool) {
		t.Helper()
		if got := contextRouteMismatch(ctx, cfgURL, activeURL, cfgProv, activeProv, false); got != want {
			t.Fatalf("contextRouteMismatch(%q, %q, %q, %q) = %v", cfgURL, activeURL, cfgProv, activeProv, got)
		}
	}
	t.Run("test_same_named_custom_provider_keeps_pin_without_configured_url", func(t *testing.T) {
		check(t, "", "http://127.0.0.1:8080/v1", "custom-local-agentw", "custom-local-agentw", false)
	})
	t.Run("test_different_provider_still_clears_pin", func(t *testing.T) {
		check(t, "", "http://127.0.0.1:8080/v1", "custom-local-agentw", "openrouter", true)
	})
	t.Run("test_explicit_base_url_mismatch_still_clears_pin", func(t *testing.T) {
		check(t, "http://127.0.0.1:8080/v1", "http://10.0.0.2:8080/v1", "custom-local-agentw", "custom-local-agentw", true)
	})
	t.Run("test_catalog_provider_rejects_non_default_runtime_url", func(t *testing.T) {
		check(t, "", "http://127.0.0.1:8080/v1", "openrouter", "openrouter", true)
	})
	if !providerDefaultRoutes(ctx, "openrouter")["https://openrouter.ai/api/v1"] {
		t.Fatalf("openrouter default routes %v", providerDefaultRoutes(ctx, "openrouter"))
	}
}

func extraBodyEntry(over map[string]any) map[string]any {
	e := map[string]any{"name": "openai", "base_url": "https://api.openai.com/v1", "extra_body": map[string]any{"service_tier": "flex"}}
	for k, v := range over {
		e[k] = v
	}
	return e
}

func TestModelMatches(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Run("test_catalog_miss_falls_back_to_model_field", func(t *testing.T) {
		e := extraBodyEntry(map[string]any{"model": "gpt-5.5", "models": map[string]any{"gpt-5.5": map[string]any{}}})
		if !customProviderModelMatches("gpt-5.5", e) || customProviderModelMatches("gpt-4o", e) {
			t.Fatal("catalog miss falls back to model field")
		}
	})
	t.Run("test_catalog_case_insensitive", func(t *testing.T) {
		if !customProviderModelMatches("gpt-5.6-terra", extraBodyEntry(map[string]any{"models": map[string]any{"GPT-5.6-Terra": map[string]any{}}})) {
			t.Fatal("catalog case insensitive")
		}
	})
}

func TestExtraBodyResolution(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	const base = "https://api.openai.com/v1"
	t.Run("test_multi_model_provider_yields_extra_body", func(t *testing.T) {
		multi := extraBodyEntry(map[string]any{"model": "gpt-5.5", "models": map[string]any{"gpt-5.5": map[string]any{}, "gpt-5.6-sol": map[string]any{}, "gpt-5.6-terra": map[string]any{}}})
		if got := customProviderExtraBodyForAgent("custom", "gpt-5.6-terra", base, []map[string]any{multi}); !reflect.DeepEqual(got, map[string]any{"service_tier": "flex"}) {
			t.Fatalf("multi model provider yields extra body %v", got)
		}
	})
	t.Run("test_non_catalog_model_gets_no_override", func(t *testing.T) {
		e := extraBodyEntry(map[string]any{"model": "gpt-5.5", "models": map[string]any{"gpt-5.5": map[string]any{}}})
		if got := customProviderExtraBodyForAgent("custom", "gpt-4o", base, []map[string]any{e}); got != nil {
			t.Fatalf("non catalog model gets no override %v", got)
		}
	})
	t.Run("test_non_custom_provider_unaffected", func(t *testing.T) {
		e := extraBodyEntry(map[string]any{"models": map[string]any{"gpt-5.6-terra": map[string]any{}}})
		if got := customProviderExtraBodyForAgent("openrouter", "gpt-5.6-terra", base, []map[string]any{e}); got != nil {
			t.Fatalf("non custom provider unaffected %v", got)
		}
	})
}

func TestCustomProviderExtraBodyPreservesCallerOverride(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{provider: "custom", model: "google/gemma-4-31b-it", baseURL: "https://example.test/v1",
		requestOverrides: map[string]any{"extra_body": map[string]any{"reasoning_effort": "low", "caller_only": true}}}
	a.mergeCustomProviderExtraBody([]map[string]any{{
		"name": "gemma", "base_url": "https://example.test/v1", "model": "google/gemma-4-31b-it",
		"extra_body": map[string]any{"enable_thinking": true, "reasoning_effort": "high"},
	}})
	want := map[string]any{"enable_thinking": true, "reasoning_effort": "low", "caller_only": true}
	if !reflect.DeepEqual(a.requestOverrides["extra_body"], want) {
		t.Fatalf("extra_body = %v", a.requestOverrides["extra_body"])
	}
}

func TestNamedCustomProviderExtraBodyMatchesProviderKey(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{provider: "custom:zai-coding-plan", model: "glm-5.2", baseURL: "https://api.z.ai/api/coding/paas/v4", requestOverrides: map[string]any{}}
	a.mergeCustomProviderExtraBody([]map[string]any{
		{"provider_key": "other-provider", "name": "Other Provider", "base_url": "https://api.z.ai/api/coding/paas/v4", "model": "glm-5.2", "extra_body": map[string]any{"enable_thinking": true}},
		{"provider_key": "zai-coding-plan", "name": "Z.AI Coding Plan", "base_url": "https://api.z.ai/api/coding/paas/v4/", "model": "glm-5.2", "extra_body": map[string]any{"enable_thinking": false}},
	})
	if !reflect.DeepEqual(a.requestOverrides, map[string]any{"extra_body": map[string]any{"enable_thinking": false}}) {
		t.Fatalf("request overrides = %v", a.requestOverrides)
	}
}

func TestAgentInitScalarHelpers(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	five := 0.85
	if got, notice := resolveCompressionThreshold(0.9, &five, "gpt-5", true); got != 0.9 || notice != nil {
		t.Fatalf("autoraise never lowers %v %v", got, notice)
	}
	high := 0.95
	if got, notice := resolveCompressionThreshold(0.9, &high, "gpt-5", true); got != 0.95 || notice["to"] != 0.95 || notice["from"] != 0.9 {
		t.Fatalf("autoraise raises %v %v", got, notice)
	}
	if got, notice := resolveCompressionThreshold(0.9, &five, "trinity", false); got != 0.85 || notice != nil {
		t.Fatalf("override unconditional %v", got)
	}
	for raw, want := range map[any]*float64{nil: nil, true: nil, "abc": nil, 0.0: nil, -3: nil} {
		if got := normalizeRunBudgetSeconds(raw); (got == nil) != (want == nil) {
			t.Errorf("run budget %v -> %v", raw, got)
		}
	}
	if got := normalizeRunBudgetSeconds("12.5"); got == nil || *got != 12.5 {
		t.Fatalf("run budget string %v", got)
	}
	if err := refuseCheckpointRequiredOnCodexAppServer(true, "codex_app_server"); err == nil || !strings.HasPrefix(err.Error(), "BLOCKED_MISSING_PREREQUISITE") {
		t.Fatal(err)
	}
	if refuseCheckpointRequiredOnCodexAppServer(true, "chat_completions") != nil || refuseCheckpointRequiredOnCodexAppServer(true, "codex_responses") != nil ||
		refuseCheckpointRequiredOnCodexAppServer(false, "codex_app_server") != nil || refuseCheckpointRequiredOnCodexAppServer(false, "") != nil {
		t.Fatal("refused a compatible pairing")
	}
	for raw, want := range map[any]int{true: 7, 3: 3, 4.0: 4, 4.5: 7, " 12 ": 12, "x": 7} {
		if got := parseConfigInt(raw, 7); got != want {
			t.Errorf("parseConfigInt(%v) = %d", raw, got)
		}
	}
	if !cfgFlag(map[string]any{"enabled": true}, "enabled", false) || cfgFlag(map[string]any{"enabled": "no"}, "enabled", true) || !cfgFlag(map[string]any{}, "enabled", true) {
		t.Fatal("cfgFlag")
	}
	if len(cfgDict(map[string]any{"s": "bad"}, "s")) != 0 || !reflect.DeepEqual(customProviderRuntimeIDs("My Local"), map[string]bool{"my-local": true, "custom:my-local": true}) {
		t.Fatal("cfgDict or runtime ids")
	}
}

func TestResolveAPIModeLadder(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	for _, c := range []struct {
		prov, base, explicit, wantMode, wantProv string
		named                                    bool
	}{
		{"", "https://chatgpt.com/backend-api/codex", "", "codex_responses", "openai-codex", false},
		{"", "https://api.x.ai/v1", "", "codex_responses", "xai", false},
		{"", "https://api.anthropic.com", "", "anthropic_messages", "anthropic", false},
		{"custom", "https://proxy.example/anthropic/", "", "anthropic_messages", "custom", true},
		{"openrouter", "https://openrouter.ai/api/v1", "codex_app_server", "codex_app_server", "openrouter", true},
		{"openrouter", "https://openrouter.ai/api/v1", "", "chat_completions", "openrouter", true},
		{"nous", "https://inference-api.nousresearch.com/v1", "", "chat_completions", "nous", true},
	} {
		a := &AIAgent{provider: c.prov}
		a.setBaseURL(c.base)
		var named *string
		if c.named {
			named = &c.prov
		}
		a.resolveAPIMode(ctx, c.explicit, named, c.base)
		if a.apiMode != c.wantMode || a.provider != c.wantProv {
			t.Errorf("%s %s -> %s %s", c.prov, c.base, a.apiMode, a.provider)
		}
	}
}

func TestNormalizeRunBudgetSecondsParams(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	f := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		raw  any
		want *float64
	}{{nil, nil}, {0, nil}, {-5, nil}, {"abc", nil}, {true, nil}, {false, nil}, {math.NaN(), nil}, {900, f(900)}, {"850", f(850)}, {0.5, f(0.5)}} {
		got := normalizeRunBudgetSeconds(c.raw)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("normalizeRunBudgetSeconds(%v) = %v", c.raw, got)
		}
	}
}

func TestResolveNoOverrideKeepsGlobal(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if effective, notice := resolveCompressionThreshold(0.50, nil, "", false); effective != 0.50 || notice != nil {
		t.Fatalf("%v %v", effective, notice)
	}
}
