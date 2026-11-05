package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Seeds the OpenRouter reasoning capability catalog the profile clamp reads
func seedOpenRouterReasoningCaps(t *testing.T, items ...map[string]any) {
	t.Helper()
	ResetReasoningCapsCatalogs()
	t.Cleanup(ResetReasoningCapsCatalogs)
	list := make([]any, len(items))
	for i, it := range items {
		list[i] = it
	}
	if !seedReasoningCaps(openRouterCatalogURL, list) {
		t.Fatal("seed rejected")
	}
}

func openrouterCapsItem(id string, efforts []any, mandatory bool) map[string]any {
	return map[string]any{"id": id, "supported_parameters": []any{"reasoning"},
		"reasoning": map[string]any{"supported_efforts": efforts, "mandatory": mandatory}}
}

func TestOpenRouterPluginProfileMatchesUpstreamRegistration(t *testing.T) {
	p := bundledPluginProfile(t, "openrouter")
	assertProfileFields(t, p, map[string]any{
		"name": "openrouter", "aliases": []string{"or"}, "env_vars": []string{"OPENROUTER_API_KEY"}, "display_name": "OpenRouter",
		"description": "OpenRouter \u2014 unified API for 200+ models", "signup_url": "https://openrouter.ai/keys",
		"base_url": "https://openrouter.ai/api/v1", "models_url": "https://openrouter.ai/api/v1/models", "api_mode": "chat_completions",
		"auth_type": "api_key", "fallback_models": []string{"anthropic/claude-sonnet-4.6", "openai/gpt-5.4", "deepseek/deepseek-chat",
			"google/gemini-3.8-flash", "google/gemini-3.7-flash", "qwen/qwen3-plus"},
		"aggregator": false, "base_url_env_var": "", "default_headers": map[string]string{}, "hidden": false,
		"supports_health_check": true, "supports_model_listing": true, "supports_vision": false, "supports_vision_tool_messages": true,
		"supports_streaming": nil, "default_max_tokens": 0, "models_dev_id": "openrouter",
	})
}

func TestOpenRouterStickySessionIDNormalizesCronTimestamp(t *testing.T) {
	p := bundledPluginProfile(t, "openrouter")
	first := p.BuildExtraBody("cron_job42_20260801_090000", map[string]any{})
	second := p.BuildExtraBody("cron_job42_20260802_090000", map[string]any{})
	if first["session_id"] != "cron_job42" || first["session_id"] != second["session_id"] {
		t.Fatalf("sticky keys %v %v", first["session_id"], second["session_id"])
	}
}

func TestOpenRouterParetoMinCodingScoreEmittedForParetoModel(t *testing.T) {
	p := bundledPluginProfile(t, "openrouter")
	body := p.BuildExtraBody("", map[string]any{"model": "openrouter/pareto-code", "openrouter_min_coding_score": 0.65})
	want := []any{map[string]any{"id": "pareto-router", "min_coding_score": 0.65}}
	if !reflect.DeepEqual(body["plugins"], want) {
		t.Fatalf("plugins = %#v", body["plugins"])
	}
	for _, bad := range []any{"x", 1.5, -0.1, ""} {
		if _, ok := p.BuildExtraBody("", map[string]any{"model": "openrouter/pareto-code", "openrouter_min_coding_score": bad})["plugins"]; ok {
			t.Fatalf("plugins emitted for score %#v", bad)
		}
	}
	if body := p.BuildExtraBody("", map[string]any{"model": "openrouter/pareto-code", "openrouter_min_coding_score": "0.5"}); !reflect.DeepEqual(body["plugins"], []any{map[string]any{"id": "pareto-router", "min_coding_score": 0.5}}) {
		t.Fatalf("string score plugins = %#v", body["plugins"])
	}
	if _, ok := p.BuildExtraBody("", map[string]any{"model": "x/y", "openrouter_min_coding_score": 0.65})["plugins"]; ok {
		t.Fatal("plugins emitted for a non pareto model")
	}
}

func TestOpenRouterGrokSessionIDSetsCacheAffinityHeader(t *testing.T) {
	p := bundledPluginProfile(t, "openrouter")
	_, tl := p.BuildAPIKwargsExtras(nil, map[string]any{"model": "x-ai/grok-4", "session_id": "sess-abc123"})
	if tl["extra_headers"].(map[string]string)["x-grok-conv-id"] != "sess-abc123" {
		t.Fatalf("top level = %#v", tl)
	}
}

func TestOpenRouterGrokConvIDNormalizesCronTimestamp(t *testing.T) {
	p := bundledPluginProfile(t, "openrouter")
	_, first := p.BuildAPIKwargsExtras(nil, map[string]any{"model": "x-ai/grok-4", "session_id": "cron_job42_20260801_090000"})
	_, second := p.BuildAPIKwargsExtras(nil, map[string]any{"model": "x-ai/grok-4", "session_id": "cron_job42_20260802_090000"})
	a := first["extra_headers"].(map[string]string)["x-grok-conv-id"]
	if a != "cron_job42" || a != second["extra_headers"].(map[string]string)["x-grok-conv-id"] {
		t.Fatalf("conv ids %v %v", first, second)
	}
}

func TestOpenRouterGrokForkScopeWinsOverSessionKey(t *testing.T) {
	p := bundledPluginProfile(t, "openrouter")
	fork := "parent" + ForkScopeSeparator + "fork1"
	_, tl := p.BuildAPIKwargsExtras(nil, map[string]any{"model": "x-ai/grok-4", "session_id": "sess", "cache_scope_id": fork})
	if tl["extra_headers"].(map[string]string)["x-grok-conv-id"] != fork {
		t.Fatalf("top level = %#v", tl)
	}
	if _, tl := p.BuildAPIKwargsExtras(nil, map[string]any{"model": "openai/gpt-5.4", "session_id": "sess"}); tl["extra_headers"] != nil {
		t.Fatalf("grok header on a non grok model %#v", tl)
	}
}

func TestOpenRouterMandatoryAnthropicVerbosityCoexistsWithGrokHeader(t *testing.T) {
	p := bundledPluginProfile(t, "openrouter")
	eb, tl := p.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(true), Effort: "high"},
		map[string]any{"supports_reasoning": true, "model": "anthropic/claude-fable-5"})
	if !reflect.DeepEqual(tl, map[string]any{"verbosity": "high"}) || len(eb) != 0 {
		t.Fatalf("extra body %#v top level %#v", eb, tl)
	}
	for _, r := range []*Reasoning{nil, {Enabled: bptr(false)}, {Enabled: bptr(true), Effort: "none"}} {
		eb, tl := p.BuildAPIKwargsExtras(r, map[string]any{"supports_reasoning": true, "model": "anthropic/claude-fable-5"})
		if len(eb) != 0 || len(tl) != 0 {
			t.Fatalf("%#v: extra body %#v top level %#v", r, eb, tl)
		}
	}
}

func TestOpenRouterSpeedTierSlugsPinEndpointsAndRewriteWireModel(t *testing.T) {
	p := bundledPluginProfile(t, "openrouter")
	for slug, pin := range openRouterEndpointPins {
		_, tl := p.BuildAPIKwargsExtras(nil, map[string]any{"model": slug})
		body := p.BuildExtraBody("", map[string]any{"model": slug, "provider_preferences": map[string]any{"ignore": []any{"deepinfra"}}})
		wire, ok := tl["model"].(string)
		if !ok {
			wire = slug
		}
		if wire != pin.base {
			t.Fatalf("%s wire model %s", slug, wire)
		}
		if !reflect.DeepEqual(body["provider"], map[string]any{"ignore": []any{"deepinfra"}, "only": pin.tags}) {
			t.Fatalf("%s provider %#v", slug, body["provider"])
		}
		var tiered []string
		for _, tag := range pin.tags {
			if strings.HasSuffix(tag, "/fast") || strings.HasSuffix(tag, "/flex") {
				tiered = append(tiered, tag)
			}
		}
		if slug == pin.base && len(tiered) != 0 || slug != pin.base && !reflect.DeepEqual(tiered, pin.tags) {
			t.Fatalf("%s tiered %v", slug, tiered)
		}
	}
	base := "openai/gpt-6-astra"
	if body := p.BuildExtraBody("", map[string]any{"model": base, "provider_preferences": map[string]any{"only": []any{"azure"}}}); !reflect.DeepEqual(body["provider"], map[string]any{"only": []any{"azure"}}) {
		t.Fatalf("explicit only on the base slug %#v", body["provider"])
	}
	if _, tl := p.BuildAPIKwargsExtras(nil, map[string]any{"model": "openai/gpt-5.6-sol"}); tl["model"] != nil {
		t.Fatalf("unpinned model rewritten %#v", tl)
	}
	if body := p.BuildExtraBody("", map[string]any{"model": "openai/gpt-5.6-sol", "provider_preferences": map[string]any{"ignore": []any{"x"}}}); !reflect.DeepEqual(body["provider"], map[string]any{"ignore": []any{"x"}}) {
		t.Fatalf("prefs %#v", body["provider"])
	}
}

func TestOpenRouterClampAppliedInBuildAPIKwargsExtras(t *testing.T) {
	p := bundledPluginProfile(t, "openrouter")
	seedOpenRouterReasoningCaps(t, openrouterCapsItem("qwen/qwen3-plus", []any{"low", "medium", "high"}, false))
	eb, _ := p.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(true), Effort: "ultra"},
		map[string]any{"supports_reasoning": true, "model": "qwen/qwen3-plus", hookCtxKey: context.Background()})
	if eb["reasoning"].(map[string]any)["effort"] != "high" {
		t.Fatalf("extra body %#v", eb)
	}
}

func TestOpenRouterNoClampWhenCatalogUnknown(t *testing.T) {
	p := bundledPluginProfile(t, "openrouter")
	seedOpenRouterReasoningCaps(t, openrouterCapsItem("a/b", []any{"low"}, false))
	eb, _ := p.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(true), Effort: "ultra"},
		map[string]any{"supports_reasoning": true, "model": "unlisted/model"})
	if eb["reasoning"].(map[string]any)["effort"] != "ultra" {
		t.Fatalf("extra body %#v", eb)
	}
}

func TestOpenRouterDisableOmittedForMandatoryRouteKeptOtherwise(t *testing.T) {
	p := bundledPluginProfile(t, "openrouter")
	seedOpenRouterReasoningCaps(t, openrouterCapsItem("z-ai/glm-5.3-flash", []any{"max", "high", "low"}, true),
		openrouterCapsItem("z-ai/glm-5.1", nil, false))
	for _, disable := range []*Reasoning{{Enabled: bptr(false)}, {Enabled: bptr(true), Effort: "none"}} {
		eb, _ := p.BuildAPIKwargsExtras(disable, map[string]any{"supports_reasoning": true, "model": "z-ai/glm-5.3-flash"})
		if _, ok := eb["reasoning"]; ok {
			t.Fatalf("%#v: reasoning sent %#v", disable, eb)
		}
	}
	eb, _ := p.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(false)}, map[string]any{"supports_reasoning": true, "model": "z-ai/glm-5.1"})
	if !reflect.DeepEqual(eb["reasoning"], map[string]any{"enabled": false}) {
		t.Fatalf("extra body %#v", eb)
	}
	eb, _ = p.BuildAPIKwargsExtras(nil, map[string]any{"supports_reasoning": true, "model": "z-ai/glm-5.1"})
	if !reflect.DeepEqual(eb["reasoning"], map[string]any{"enabled": true, "effort": "medium"}) {
		t.Fatalf("default reasoning %#v", eb)
	}
}
