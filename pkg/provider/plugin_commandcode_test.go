package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestDeepSeekPluginProfileMatchesUpstreamRegistration(t *testing.T) {
	p := bundledPluginProfile(t, "deepseek")
	assertProfileFields(t, p, map[string]any{
		"name": "deepseek", "aliases": []string{"deepseek-chat", "deep-seek"}, "env_vars": []string{"DEEPSEEK_API_KEY"},
		"display_name": "DeepSeek", "description": "DeepSeek — native DeepSeek API", "signup_url": "https://platform.deepseek.com/",
		"fallback_models": []string{"deepseek-v4-pro", "deepseek-flash"}, "base_url": "https://api.deepseek.com/v1",
		"default_aux_model": "deepseek-flash", "base_url_env_var": "", "aggregator": false, "api_mode": "chat_completions",
	})
	if !reflect.DeepEqual(p.UnsupportedResponseFormats, []string{"json_schema"}) {
		t.Fatalf("formats %v", p.UnsupportedResponseFormats)
	}
	for _, m := range p.FallbackModels {
		if normalizeForDeepseek(m) != m {
			t.Fatalf("fallback %s is not a current id", m)
		}
	}
}

func TestDeepSeekThinkingWireShape(t *testing.T) {
	p := bundledPluginProfile(t, "deepseek")
	enabled := map[string]any{"thinking": map[string]any{"type": "enabled"}}
	disabled := map[string]any{"thinking": map[string]any{"type": "disabled"}}
	type tc struct {
		r      *Reasoning
		model  string
		eb, tl map[string]any
	}
	cases := []tc{{nil, "deepseek-v4-pro", enabled, map[string]any{}}}
	for _, e := range []string{"low", "medium", "high"} {
		cases = append(cases, tc{effortCfg(e), "deepseek-v4-pro", enabled, map[string]any{"reasoning_effort": e}})
	}
	for _, e := range []string{"xhigh", "max", "MAX", "  Max  "} {
		cases = append(cases, tc{effortCfg(e), "deepseek-v4-pro", enabled, map[string]any{"reasoning_effort": "max"}})
	}
	cases = append(cases, tc{&Reasoning{Enabled: bptr(false)}, "deepseek-v4-pro", disabled, map[string]any{}},
		tc{&Reasoning{Enabled: bptr(false), Effort: "high"}, "deepseek-v4-pro", disabled, map[string]any{}},
		tc{effortCfg("garbage"), "deepseek-v4-pro", enabled, map[string]any{}},
		tc{effortCfg(""), "deepseek-v4-pro", enabled, map[string]any{}})
	for _, r := range []*Reasoning{{Effort: "none"}, {Effort: "false"}, {Effort: "disabled"}, {Effort: "NONE"}, {Enabled: bptr(true), Effort: "none"}} {
		cases = append(cases, tc{r, "deepseek-v4.1-flash-expires-on-0910", disabled, map[string]any{}})
	}
	for _, m := range []string{"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v4-future-variant", "DEEPSEEK-V4-PRO", "deepseek-flash", "DEEPSEEK-FLASH"} {
		cases = append(cases, tc{nil, m, enabled, map[string]any{}})
	}
	for _, m := range []string{"deepseek-v3-0324", "deepseek-v3.1", "", "deepseek-unknown"} {
		cases = append(cases, tc{effortCfg("high"), m, map[string]any{}, map[string]any{}})
	}
	for _, c := range cases {
		eb, tl := p.BuildAPIKwargsExtras(c.r, map[string]any{"model": c.model})
		assertExtras(t, c.model+" "+reasoningLabel(c.r), eb, tl, c.eb, c.tl)
	}
}

func reasoningLabel(r *Reasoning) string {
	if r == nil {
		return "nil"
	}
	return pyRepr(reasoningConfigMap(r))
}

func TestCommandCodePluginProfilesMatchUpstreamRegistration(t *testing.T) {
	chat := bundledPluginProfile(t, "commandcode")
	assertProfileFields(t, chat, map[string]any{
		"name": "commandcode", "aliases": []string{"commandcode-chat"}, "api_mode": "chat_completions",
		"env_vars": []string{"COMMANDCODE_API_KEY", "COMMANDCODE_BASE_URL"}, "display_name": "CommandCode",
		"description": "CommandCode — 20+ models via OpenAI-compatible API", "signup_url": "https://commandcode.ai/",
		"base_url": "https://api.commandcode.ai/provider/v1", "models_url": "https://api.commandcode.ai/provider/v1/models",
		"default_aux_model": "deepseek/deepseek-v4-flash", "base_url_env_var": "",
	})
	anth := bundledPluginProfile(t, "commandcode-anthropic")
	assertProfileFields(t, anth, map[string]any{
		"name": "commandcode-anthropic", "aliases": []string{"commandcode-claude"}, "api_mode": "anthropic_messages",
		"env_vars": []string{"COMMANDCODE_API_KEY", "COMMANDCODE_ANTHROPIC_BASE_URL"}, "display_name": "CommandCode (Anthropic)",
		"description": "CommandCode — Claude models via Anthropic Messages API", "signup_url": "https://commandcode.ai/",
		"base_url": "https://api.commandcode.ai/provider/v1", "models_url": "https://api.commandcode.ai/provider/v1/models",
		"fallback_models":   []string{"claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5-20251001"},
		"default_aux_model": "claude-haiku-4-5-20251001",
	})
	for _, m := range anth.FallbackModels {
		if !strings.HasPrefix(m, "claude-") {
			t.Fatalf("anthropic fallback %s", m)
		}
	}
}

func TestCommandCodeDeepSeekReasoningWireControls(t *testing.T) {
	cc := bundledPluginProfile(t, "commandcode")
	eb, tl := cc.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(false)}, map[string]any{"model": "deepseek/deepseek-v4-flash"})
	assertExtras(t, "disabled", eb, tl, map[string]any{"thinking": map[string]any{"type": "disabled"}}, map[string]any{})
	ds, _ := GetProviderProfile(context.Background(), "deepseek")
	rc := effortCfg("low")
	wantEB, wantTL := ds.BuildAPIKwargsExtras(rc, map[string]any{"model": "deepseek-v4.1-flash"})
	if wantTL["reasoning_effort"] != "low" {
		t.Fatalf("native %v", wantTL)
	}
	eb, tl = cc.BuildAPIKwargsExtras(rc, map[string]any{"model": "deepseek/deepseek-v4.1-flash"})
	assertExtras(t, "low", eb, tl, wantEB, wantTL)
	eb, tl = cc.BuildAPIKwargsExtras(rc, map[string]any{"model": "Qwen/Qwen3.7-Max"})
	assertExtras(t, "qwen", eb, tl, map[string]any{}, map[string]any{})
	anth := bundledPluginProfile(t, "commandcode-anthropic")
	eb, tl = anth.BuildAPIKwargsExtras(rc, map[string]any{"model": "deepseek/deepseek-v4.1-flash"})
	assertExtras(t, "anthropic inherits", eb, tl, wantEB, wantTL)
}

func serveCommandCodeModels(t *testing.T, models string, paths *[]string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.Path)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("credential sent to the public listing")
		}
		_, _ = w.Write([]byte(`{"data": ` + models + `}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestCommandCodeCustomBaseURLRedirectsFetch(t *testing.T) {
	var paths []string
	base := serveCommandCodeModels(t, `[{"id": "proxied/model-x"}]`, &paths)
	cc := bundledPluginProfile(t, "commandcode")
	if got, ok := cc.FetchModels(context.Background(), "k", base, 0); !ok || !reflect.DeepEqual(got, []string{"proxied/model-x"}) {
		t.Fatalf("models %v %v", got, ok)
	}
	if !reflect.DeepEqual(paths, []string{"/models"}) {
		t.Fatalf("paths %v", paths)
	}
}

func TestCommandCodeCustomBaseURLRedirectsAnthropicFetch(t *testing.T) {
	var paths []string
	base := serveCommandCodeModels(t, `[{"id": "claude-sonnet-4-6"}, {"id": "deepseek/deepseek-v4-pro"}]`, &paths)
	anth := bundledPluginProfile(t, "commandcode-anthropic")
	if got, ok := anth.FetchModels(context.Background(), "k", base, 0); !ok || !reflect.DeepEqual(got, []string{"claude-sonnet-4-6"}) {
		t.Fatalf("models %v %v", got, ok)
	}
}

func TestCommandCodeDefaultBaseURLHitsDefaultEndpoint(t *testing.T) {
	for _, base := range []string{commandcodeBase + "/", commandcodeBase, "", "  "} {
		if got := commandcodeModelsURLFor(base); got != commandcodeModelsURL {
			t.Fatalf("%q -> %s", base, got)
		}
	}
}

func TestCommandCodeFetchModelsFailureIsNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[{"id": "x"}]`)) }))
	defer srv.Close()
	cc := bundledPluginProfile(t, "commandcode")
	if got, ok := cc.FetchModels(context.Background(), "", srv.URL, 0); ok || got != nil {
		t.Fatalf("list payload %v %v", got, ok)
	}
}
