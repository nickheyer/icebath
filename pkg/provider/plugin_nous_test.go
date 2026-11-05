package provider

import (
	"context"
	"reflect"
	"slices"
	"testing"
)

// Primes the Portal reasoning capability cache with the upstream fixture entries
func seedNousPortalCatalog(t *testing.T) Profile {
	t.Helper()
	p := bundledPluginProfile(t, "nous")
	ResetReasoningCapsCatalogs()
	t.Cleanup(ResetReasoningCapsCatalogs)
	items := []any{
		map[string]any{"id": "deepseek/deepseek-v4-pro", "supported_parameters": []any{"reasoning"},
			"reasoning": map[string]any{"supported_efforts": []any{"xhigh", "high"}, "mandatory": false}},
		map[string]any{"id": "arcee-ai/trinity-large-thinking", "supported_parameters": []any{"reasoning"},
			"reasoning": map[string]any{"supported_efforts": nil, "mandatory": true}},
		map[string]any{"id": "moonshotai/kimi-k3-instruct", "supported_parameters": []any{}},
	}
	if !seedReasoningCaps(NousCatalogURL(context.Background()), items) {
		t.Fatal("seed rejected")
	}
	return p
}

func nousExtras(p Profile, r *Reasoning, supports bool, model string) (map[string]any, map[string]any) {
	return p.BuildAPIKwargsExtras(r, map[string]any{"supports_reasoning": supports, "model": model})
}

func TestNousPluginProfileMatchesUpstreamRegistration(t *testing.T) {
	p := bundledPluginProfile(t, "nous")
	assertProfileFields(t, p, map[string]any{"name": "nous", "aliases": []string{"nous-portal", "nousresearch"},
		"env_vars": []string{"NOUS_API_KEY"}, "display_name": "Nous Research", "description": "Nous Research — Hermes model family",
		"signup_url": "https://nousresearch.com/", "fallback_models": []string{"hermes-3-405b", "hermes-3-70b"},
		"base_url": "https://inference-api.nousresearch.com/v1", "auth_type": "oauth_device_code", "base_url_env_var": ""})
}

func TestNousReasoningWireShape(t *testing.T) {
	p := seedNousPortalCatalog(t)
	off := &Reasoning{Enabled: bptr(false)}
	extra, top := nousExtras(p, off, true, "deepseek/deepseek-v4-pro")
	if !reflect.DeepEqual(extra, map[string]any{"reasoning": map[string]any{"enabled": false}}) || len(top) != 0 {
		t.Fatalf("optional model disable = %v %v", extra, top)
	}
	for _, model := range []string{"arcee-ai/trinity-large-thinking", "private/unlisted-route", "moonshotai/kimi-k3-instruct"} {
		if extra, _ := nousExtras(p, off, true, model); extra["reasoning"] != nil {
			t.Fatalf("%s disable kept = %v", model, extra)
		}
	}
	for _, model := range []string{"deepseek/deepseek-v4-pro", "arcee-ai/trinity-large-thinking", "private/unlisted-route"} {
		extra, _ := nousExtras(p, &Reasoning{Enabled: bptr(true), Effort: "high"}, true, model)
		if !reflect.DeepEqual(extra["reasoning"], map[string]any{"enabled": true, "effort": "high"}) {
			t.Fatalf("%s enabled = %v", model, extra)
		}
	}
	if extra, _ := nousExtras(p, nil, true, "deepseek/deepseek-v4-pro"); !reflect.DeepEqual(extra["reasoning"], map[string]any{"enabled": true, "effort": "medium"}) {
		t.Fatalf("unset = %v", extra)
	}
	if extra, top := nousExtras(p, off, false, "deepseek/deepseek-v4-pro"); len(extra) != 0 || len(top) != 0 {
		t.Fatalf("no reasoning support = %v %v", extra, top)
	}
	cfg := &Reasoning{Enabled: bptr(false)}
	nousExtras(p, cfg, true, "deepseek/deepseek-v4-pro")
	if !reflect.DeepEqual(cfg, &Reasoning{Enabled: bptr(false)}) {
		t.Fatalf("caller config mutated = %+v", cfg)
	}
}

func TestNousStickySessionIDNormalizesCronTimestamp(t *testing.T) {
	p := bundledPluginProfile(t, "nous")
	first := p.BuildExtraBody("cron_job42_20260801_090000", map[string]any{})
	second := p.BuildExtraBody("cron_job42_20260802_090000", map[string]any{})
	if first["session_id"] != "cron_job42" || first["session_id"] != second["session_id"] {
		t.Fatalf("sticky = %v %v", first["session_id"], second["session_id"])
	}
}

func TestNousExtraBodyIgnoresProviderPreferences(t *testing.T) {
	p := bundledPluginProfile(t, "nous")
	body := p.BuildExtraBody("", map[string]any{"provider_preferences": map[string]any{"allow": []any{"anthropic"}, "sort": "price"}})
	if _, ok := body["provider"]; ok || body["tags"] == nil {
		t.Fatalf("body = %v", body)
	}
	body = p.BuildExtraBody("", map[string]any{"data_use": map[string]bool{"zero_retention": true}})
	if !reflect.DeepEqual(body["provider"], map[string]any{"data_collection": "deny", "zdr": true}) {
		t.Fatalf("data use body = %v", body)
	}
	body = p.BuildExtraBody("", map[string]any{"data_use": []string{"no_training"}})
	if !reflect.DeepEqual(body["provider"], map[string]any{"data_collection": "deny"}) {
		t.Fatalf("data use body = %v", body)
	}
}

func TestNousStickyKeyMatchesConversationTag(t *testing.T) {
	p := bundledPluginProfile(t, "nous")
	ctx := WithPortalConversationContext(context.Background(), "root-conversation")
	body := p.BuildExtraBody("segment-after-compaction", map[string]any{hookCtxKey: ctx})
	if body["session_id"] != "root-conversation" || !slices.Contains(body["tags"].([]any), any(conversationTag("root-conversation"))) {
		t.Fatalf("body = %v", body)
	}
	if aux := p.BuildExtraBody("", map[string]any{hookCtxKey: ctx}); aux["session_id"] != "root-conversation" {
		t.Fatalf("aux body = %v", aux)
	}
	scoped := p.BuildExtraBody("segment", map[string]any{hookCtxKey: WithPortalAffinityScope(ctx, "declared-chat")})
	if scoped["session_id"] != "declared-chat" || !slices.Contains(scoped["tags"].([]any), any(conversationTag("root-conversation"))) {
		t.Fatalf("scoped body = %v", scoped)
	}
}
