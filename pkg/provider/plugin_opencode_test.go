package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func effortCfg(effort string) *Reasoning { return &Reasoning{Enabled: bptr(true), Effort: effort} }

func assertExtras(t *testing.T, label string, gotEB, gotTL, wantEB, wantTL map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(gotEB, wantEB) || !reflect.DeepEqual(gotTL, wantTL) {
		t.Fatalf("%s: extra body %#v top level %#v, want %#v %#v", label, gotEB, gotTL, wantEB, wantTL)
	}
}

func TestOpencodePluginProfilesMatchUpstreamRegistration(t *testing.T) {
	zen := bundledPluginProfile(t, "opencode-zen")
	assertProfileFields(t, zen, map[string]any{
		"name": "opencode-zen", "aliases": []string{"opencode", "opencode_zen", "zen"}, "env_vars": []string{"OPENCODE_ZEN_API_KEY"},
		"base_url": "https://opencode.ai/zen/v1", "default_aux_model": "gemini-3-flash", "api_mode": "chat_completions", "auth_type": "api_key",
		"display_name": "", "description": "", "aggregator": false, "base_url_env_var": "", "supports_vision_tool_messages": true,
	})
	goP := bundledPluginProfile(t, "opencode-go")
	assertProfileFields(t, goP, map[string]any{
		"name": "opencode-go", "aliases": []string{"opencode_go", "go", "opencode-go-sub"}, "env_vars": []string{"OPENCODE_GO_API_KEY"},
		"base_url": "https://opencode.ai/zen/go/v1", "default_aux_model": "glm-5", "supports_vision_tool_messages": false,
		"aggregator": false, "base_url_env_var": "", "default_max_tokens": 0,
	})
	want := map[string]string{"HTTP-Referer": attributionReferer, "X-Title": attributionTitle, "User-Agent": UserAgent("IcebathAgent")}
	for _, p := range []Profile{zen, goP} {
		if got := p.DefaultHeaders(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s headers %v", p.Name, got)
		}
	}
	if goP.GetMaxTokens("xiaomi/MiMo-V2.5-Pro") != 131072 || goP.GetMaxTokens("glm-5") != 0 {
		t.Fatal("opencode-go max tokens table")
	}
}

func TestOpencodeZenOxAlphaReasoning(t *testing.T) {
	zen := bundledPluginProfile(t, "opencode-zen")
	eb, tl := zen.BuildAPIKwargsExtras(effortCfg("max"), map[string]any{"model": "x-preview-f-free"})
	assertExtras(t, "max", eb, tl, map[string]any{}, map[string]any{"reasoning_effort": "max"})
	for _, r := range []*Reasoning{nil, {Enabled: bptr(false)}} {
		eb, tl := zen.BuildAPIKwargsExtras(r, map[string]any{"model": "x-preview-f-free"})
		assertExtras(t, "unset or disabled", eb, tl, map[string]any{}, map[string]any{})
	}
	eb, tl = zen.BuildAPIKwargsExtras(effortCfg("max"), map[string]any{"model": "gemini-3-flash"})
	assertExtras(t, "other model", eb, tl, map[string]any{}, map[string]any{})
	for requested, expected := range map[string]string{"medium": "low", "xhigh": "max"} {
		_, tl := zen.BuildAPIKwargsExtras(effortCfg(requested), map[string]any{"model": "x-preview-f-free"})
		assertExtras(t, requested, map[string]any{}, tl, map[string]any{}, map[string]any{"reasoning_effort": expected})
	}
}

func TestOpencodeGoKimiReasoning(t *testing.T) {
	goP := bundledPluginProfile(t, "opencode-go")
	cases := []struct {
		r      *Reasoning
		model  string
		eb, tl map[string]any
	}{
		{effortCfg("high"), "kimi-k2.6", map[string]any{}, map[string]any{"reasoning_effort": "high"}},
		{&Reasoning{Enabled: bptr(false)}, "kimi-k2.6", map[string]any{"thinking": map[string]any{"type": "disabled"}}, map[string]any{}},
		{effortCfg("minimal"), "kimi-k2.6", map[string]any{}, map[string]any{"reasoning_effort": "low"}},
		{effortCfg("xhigh"), "moonshotai/kimi-k2.6", map[string]any{}, map[string]any{"reasoning_effort": "high"}},
		{effortCfg("max"), "moonshotai/kimi-k2.6", map[string]any{}, map[string]any{"reasoning_effort": "high"}},
		{effortCfg("low"), "kimi-k2.5", map[string]any{}, map[string]any{"reasoning_effort": "low"}},
		{effortCfg("medium"), "kimi-k2.5", map[string]any{}, map[string]any{"reasoning_effort": "medium"}},
		{nil, "kimi-k2.6", map[string]any{}, map[string]any{}},
	}
	for _, c := range cases {
		eb, tl := goP.BuildAPIKwargsExtras(c.r, map[string]any{"model": c.model})
		assertExtras(t, c.model, eb, tl, c.eb, c.tl)
	}
}

func TestOpencodeGoDeepSeekThinking(t *testing.T) {
	goP := bundledPluginProfile(t, "opencode-go")
	for _, effort := range []string{"xhigh", "max"} {
		eb, tl := goP.BuildAPIKwargsExtras(effortCfg(effort), map[string]any{"model": "deepseek/deepseek-v4-pro"})
		assertExtras(t, effort, eb, tl, map[string]any{}, map[string]any{"reasoning_effort": "max"})
	}
	for _, model := range []string{"deepseek-flash", "deepseek/deepseek-flash"} {
		_, tl := goP.BuildAPIKwargsExtras(effortCfg("high"), map[string]any{"model": model})
		assertExtras(t, model, map[string]any{}, tl, map[string]any{}, map[string]any{"reasoning_effort": "high"})
		eb, tl := goP.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(false)}, map[string]any{"model": model})
		assertExtras(t, model, eb, tl, map[string]any{"thinking": map[string]any{"type": "disabled"}}, map[string]any{})
	}
}

func TestOpencodeGoGLM52AliasSpellingsRecognized(t *testing.T) {
	goP := bundledPluginProfile(t, "opencode-go")
	for _, model := range []string{"glm-5-2", "glm-5p2"} {
		_, tl := goP.BuildAPIKwargsExtras(effortCfg("max"), map[string]any{"model": model})
		assertExtras(t, model, map[string]any{}, tl, map[string]any{}, map[string]any{"reasoning_effort": "max"})
	}
	eb, tl := goP.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(true), Effort: "none"}, map[string]any{"model": "glm-5.2"})
	assertExtras(t, "none", eb, tl, map[string]any{}, map[string]any{})
}

func TestOpencodeGoNonTargetModelsEmitNothing(t *testing.T) {
	goP := bundledPluginProfile(t, "opencode-go")
	for _, model := range []string{"glm-5.1", "glm-5", "qwen3.6-plus", "minimax-m2.7", "deepseek-v3.1", "deepseek-chat", ""} {
		eb, tl := goP.BuildAPIKwargsExtras(effortCfg("high"), map[string]any{"model": model})
		assertExtras(t, model, eb, tl, map[string]any{}, map[string]any{})
	}
	eb, tl := goP.BuildAPIKwargsExtras(effortCfg("high"), map[string]any{})
	assertExtras(t, "no model", eb, tl, map[string]any{}, map[string]any{})
}

func TestOpencodeGoPlanWindowsReachUsageThroughProfileHook(t *testing.T) {
	var urls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		urls = append(urls, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"usage": {
			"rolling": {"status": "ok", "percent": 3, "resetsAt": "2026-09-16T21:44:55.176Z"},
			"weekly": {"status": "ok", "percent": 2, "resetsAt": "2026-09-21T00:00:00.176Z"},
			"monthly": {"status": "ok", "percent": 2, "resetsAt": "2026-10-13T02:13:38.176Z"}}}`))
	}))
	defer srv.Close()
	saved := opencodeGoUsageURL
	opencodeGoUsageURL = srv.URL + "/zen/go/v1/usage"
	t.Cleanup(func() { opencodeGoUsageURL = saved })
	goP := bundledPluginProfile(t, "opencode-go")
	t.Setenv("OPENCODE_GO_API_KEY", "sk-test")
	snap, err := goP.FetchAccountUsage(context.Background(), "", "")
	if err != nil || snap == nil || snap.Provider != "opencode-go" || snap.Source != "go_usage_api" || snap.Title != "Account limits" {
		t.Fatalf("snapshot %#v err %v", snap, err)
	}
	var got [][2]any
	for _, w := range snap.Windows {
		got = append(got, [2]any{w.Label, *w.UsedPercent})
	}
	if !reflect.DeepEqual(got, [][2]any{{"Rolling window", 3.0}, {"Weekly", 2.0}, {"Monthly", 2.0}}) {
		t.Fatalf("windows %v", got)
	}
	if want := time.Date(2026, 9, 16, 21, 44, 55, 176000000, time.UTC); !snap.Windows[0].ResetAt.Equal(want) {
		t.Fatalf("reset %v", snap.Windows[0].ResetAt)
	}
	if !reflect.DeepEqual(urls, []string{"/zen/go/v1/usage"}) {
		t.Fatalf("urls %v", urls)
	}
	t.Setenv("OPENCODE_GO_API_KEY", "")
	if snap, err := goP.FetchAccountUsage(context.Background(), "", ""); snap != nil || err == nil || !strings.Contains(err.Error(), "No usable credentials") {
		t.Fatalf("keyless snapshot %#v %v", snap, err)
	}
}
