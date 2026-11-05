package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func extrasOf(t *testing.T, name string, r *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	if kw == nil {
		kw = map[string]any{}
	}
	return bundledPluginProfile(t, name).BuildAPIKwargsExtras(r, kw)
}

func TestSliceThreeProfilesMatchUpstreamRegistration(t *testing.T) {
	cases := map[string]map[string]any{
		"deepinfra": {"display_name": "DeepInfra", "description": "DeepInfra — 100+ open models, pay-per-use",
			"env_vars": []string{"DEEPINFRA_API_KEY", "DEEPINFRA_BASE_URL"}, "base_url": "https://api.deepinfra.com/v1/openai",
			"aggregator": false, "default_aux_model": "deepseek-ai/DeepSeek-V4-Flash", "default_max_tokens": 0,
			"aliases": []string{"deep-infra", "deepinfra-ai"}},
		"minimax-oauth": {"api_mode": "anthropic_messages", "auth_type": "oauth_external", "default_aux_model": "MiniMax-M2.7",
			"display_name": "MiniMax (OAuth)", "env_vars": []string{}},
		"alibaba-token-plan-cn": {"env_vars": []string{"ALIBABA_TOKEN_PLAN_CN_API_KEY", "ALIBABA_TOKEN_PLAN_API_KEY", "ALIBABA_TOKEN_PLAN_CN_BASE_URL"},
			"base_url": "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", "aliases": []string{"dashscope-token-plan-cn"},
			"models_dev_id": "alibaba-token-plan-cn"},
		"anthropic": {"api_mode": "anthropic_messages", "default_aux_model": "claude-haiku-4-5-20251001",
			"env_vars": []string{"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}},
		"upstage":              {"display_name": "Upstage Solar", "fallback_models": []string{"solar-pro3"}, "aliases": []string{"solar"}},
		"nebius-token-factory": {"models_url": "https://api.tokenfactory.nebius.com/v1/models?verbose=true", "aggregator": false},
		"novita":               {"aggregator": false, "default_aux_model": "deepseek/deepseek-v3-0324"},
		"lmstudio": {"display_name": "LM Studio", "base_url": "", "supports_model_listing": false, "supports_health_check": false,
			"env_vars": []string{"LM_API_KEY", "LM_BASE_URL"}},
		"tencent-tokenhub": {"display_name": "Tencent TokenHub", "base_url": "", "supports_health_check": false},
		"nvidia":           {"default_max_tokens": 16384, "display_name": "NVIDIA NIM"},
		"vertex":           {"auth_type": "vertex", "supports_model_listing": false, "env_vars": []string{}},
		"bedrock":          {"api_mode": "bedrock_converse", "auth_type": "aws_sdk", "supports_model_listing": false},
		"azure-foundry":    {"base_url": "", "display_name": "Azure Foundry"},
		"xiaomi":           {"supports_health_check": false, "supports_vision": true, "supports_vision_tool_messages": false},
		"xai":              {"api_mode": "codex_responses", "default_headers": map[string]string{}},
		"openai-codex":     {"api_mode": "codex_responses", "auth_type": "oauth_external", "aliases": []string{"codex", "openai_codex"}},
		"stepfun":          {"default_aux_model": "step-3.5-flash", "aliases": []string{"step", "stepfun-coding-plan"}},
		"solstice": {"hidden": true, "auth_type": "oauth_external", "supports_vision": true, "supports_health_check": false,
			"supports_model_listing": false, "default_aux_model": "gemini-flash-lite-latest", "aliases": []string{"solstice-oauth"}},
	}
	for name, want := range cases {
		assertProfileFields(t, bundledPluginProfile(t, name), want)
	}
	if ua := bundledPluginProfile(t, "xai").DefaultHeaders()["User-Agent"]; ua != UserAgent("Icebath-Agent") {
		t.Fatalf("xai user agent %q", ua)
	}
	if ua := bundledPluginProfile(t, "gmi").DefaultHeaders()["User-Agent"]; ua != UserAgent("IcebathAgent") {
		t.Fatalf("gmi user agent %q", ua)
	}
	if bundledPluginProfile(t, "solstice").GetMaxTokens("gemini-3.1-pro") != 0 {
		t.Fatal("solstice must leave output caps to the backend")
	}
}

func TestDeepInfraReasoningTopLevelEffort(t *testing.T) {
	cases := []struct {
		r    *Reasoning
		want map[string]any
	}{
		{effortCfg("high"), map[string]any{"reasoning_effort": "high"}},
		{effortCfg("xhigh"), map[string]any{"reasoning_effort": "xhigh"}},
		{effortCfg("ultra"), map[string]any{"reasoning_effort": "max"}},
		{&Reasoning{Enabled: bptr(false)}, map[string]any{"reasoning_effort": "none"}},
		{effortCfg("none"), map[string]any{"reasoning_effort": "none"}},
		{nil, map[string]any{}},
		{&Reasoning{Enabled: bptr(true)}, map[string]any{}},
		{effortCfg("future-tier"), map[string]any{}},
	}
	for _, c := range cases {
		extra, top := extrasOf(t, "deepinfra", c.r, map[string]any{"supports_reasoning": false, "model": "deepseek-ai/DeepSeek-V4.1-Flash"})
		assertExtras(t, "deepinfra", extra, top, map[string]any{}, c.want)
	}
}

func TestDeepInfraVisionDefaultIsKeyGatedAndChatTagged(t *testing.T) {
	p := bundledPluginProfile(t, "deepinfra")
	calls := 0
	orig := deepinfraModelsByTagFn
	t.Cleanup(func() { deepinfraModelsByTagFn = orig })
	deepinfraModelsByTagFn = func(_ context.Context, tag string, _ time.Duration, _ bool) ([]map[string]any, bool) {
		calls++
		if tag != "chat" {
			t.Fatalf("tag %q", tag)
		}
		return []map[string]any{{"id": "text-only", "metadata": map[string]any{"tags": []any{"chat"}}},
			{"id": "", "metadata": map[string]any{"tags": []any{"vision"}}},
			{"id": "meta-llama/Vision-11B", "metadata": map[string]any{"tags": []any{"chat", "vision"}}}}, true
	}
	t.Setenv("DEEPINFRA_API_KEY", "")
	if got := p.DefaultVisionModel(context.Background()); got != "" || calls != 0 {
		t.Fatalf("keyless box fetched (%q, %d calls)", got, calls)
	}
	t.Setenv("DEEPINFRA_API_KEY", "di-key")
	if got := p.DefaultVisionModel(context.Background()); got != "meta-llama/Vision-11B" {
		t.Fatalf("vision default %q", got)
	}
	deepinfraModelsByTagFn = func(context.Context, string, time.Duration, bool) ([]map[string]any, bool) { return nil, false }
	if got := p.DefaultVisionModel(context.Background()); got != "" {
		t.Fatalf("failed fetch gave %q", got)
	}
}

func TestMiniMaxProfiles(t *testing.T) {
	for _, name := range []string{"minimax", "minimax-cn", "minimax-oauth"} {
		if bundledPluginProfile(t, name).DefaultAuxModel == "" {
			t.Fatalf("%s advertises no aux model", name)
		}
	}
	extra, top := extrasOf(t, "minimax", nil, map[string]any{"model": "MiniMax-M3", "base_url": "https://api.minimax.io/v1"})
	assertExtras(t, "m3 default", extra, top, map[string]any{"reasoning_split": true}, map[string]any{})
	extra, top = extrasOf(t, "minimax", effortCfg("medium"), map[string]any{"model": "MiniMax-M3", "base_url": "https://api.minimax.io/v1/"})
	assertExtras(t, "m3 adaptive", extra, top, map[string]any{"reasoning_split": true, "thinking": map[string]any{"type": "adaptive"}}, map[string]any{})
	extra, _ = extrasOf(t, "minimax", &Reasoning{Enabled: bptr(false)}, map[string]any{"model": "minimax/MiniMax-M3", "base_url": "https://API.minimax.io/v1"})
	if !reflect.DeepEqual(extra["thinking"], map[string]any{"type": "disabled"}) {
		t.Fatalf("disabled m3 %v", extra)
	}
	for _, c := range [][2]string{{"MiniMax-M2.7", "https://api.minimax.io/v1"}, {"MiniMax-M3", "https://api.minimax.io/anthropic"}, {"MiniMax-M3", "https://api.minimaxi.com/v1"}} {
		extra, top := extrasOf(t, "minimax", effortCfg("high"), map[string]any{"model": c[0], "base_url": c[1]})
		assertExtras(t, c[0]+" "+c[1], extra, top, map[string]any{}, map[string]any{})
	}
	for _, alias := range []string{"minimax_oauth", "minimax-portal", "minimax-global"} {
		if p, ok := GetProviderProfile(context.Background(), alias); !ok || p.Name != "minimax-oauth" {
			t.Fatalf("alias %s resolved to %q", alias, p.Name)
		}
	}
}

func TestAlibabaRegionalAndTokenPlanProfilesResolveInAuthRegistry(t *testing.T) {
	h := newTestHome(t)
	t.Setenv("DASHSCOPE_API_KEY", "sk-test")
	t.Setenv("ALIBABA_CODING_PLAN_API_KEY", "sk-test")
	t.Setenv("ALIBABA_TOKEN_PLAN_API_KEY", "sk-test")
	env := testEnv(t, h)
	ctx := context.Background()
	for _, pid := range []string{"alibaba-cn", "alibaba-coding-plan-cn", "alibaba-token-plan", "alibaba-token-plan-cn"} {
		row := registryLookup(ctx, pid)
		if row == nil {
			t.Fatalf("%s missing from the auth registry", pid)
		}
		if got, err := resolveProvider(env, pid); err != nil || got != pid {
			t.Fatalf("resolveProvider(%s) = %q, %v", pid, got, err)
		}
		if p := bundledPluginProfile(t, pid); row.BaseURL != p.BaseURL {
			t.Fatalf("%s registry base %q, profile %q", pid, row.BaseURL, p.BaseURL)
		}
	}
	for _, name := range []string{"alibaba", "alibaba-coding-plan-cn"} {
		extra, _ := extrasOf(t, name, nil, map[string]any{"model": "qwen3.6-plus"})
		if !reflect.DeepEqual(extra, dashscopePreserveThinkingExtras("qwen3.6-plus")) {
			t.Fatalf("%s extras %v", name, extra)
		}
	}
}

func TestAnthropicProfileFetchModelsFollowsCursorPages(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("x-api-key")+"|"+r.Header.Get("anthropic-version")+"|"+r.URL.Query().Get("after_id"))
		if r.URL.Query().Get("after_id") == "" {
			io.WriteString(w, `{"data":[{"id":"claude-a"},{"id":"claude-b"},{"name":"no-id"}],"has_more":true,"last_id":"claude-b"}`)
			return
		}
		io.WriteString(w, `{"data":[{"id":"claude-b"},{"id":"claude-c"}],"has_more":false,"last_id":"claude-c"}`)
	}))
	defer srv.Close()
	p := bundledPluginProfile(t, "anthropic")
	if _, ok := p.FetchModels(context.Background(), "", srv.URL, time.Second); ok {
		t.Fatal("no key must list nothing")
	}
	got, ok := p.FetchModels(context.Background(), "sk-ant", srv.URL, time.Second)
	if !ok || !reflect.DeepEqual(got, []string{"claude-a", "claude-b", "claude-c"}) {
		t.Fatalf("models %v %v", got, ok)
	}
	if len(keys) != 2 || keys[0] != "sk-ant|2023-06-01|" || keys[1] != "sk-ant|2023-06-01|claude-b" {
		t.Fatalf("requests %v", keys)
	}
}

func TestUpstageReasoning(t *testing.T) {
	p := bundledPluginProfile(t, "upstage")
	for _, m := range p.FallbackModels {
		if strings.Contains(m, "solar-mini") || strings.Contains(m, "syn-pro") {
			t.Fatalf("non agentic fallback %s", m)
		}
	}
	for _, e := range []string{"low", "medium", "high"} {
		extra, top := p.BuildAPIKwargsExtras(effortCfg(e), map[string]any{"model": "solar-pro3"})
		assertExtras(t, e, extra, top, map[string]any{}, map[string]any{"reasoning_effort": e})
	}
	if _, top := p.BuildAPIKwargsExtras(effortCfg("hyperthink"), map[string]any{"model": "solar-pro3"}); top["reasoning_effort"] != "high" {
		t.Fatalf("unknown effort %v", top)
	}
	if _, top := p.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(false), Effort: "high"}, map[string]any{"model": "solar-pro3"}); len(top) != 0 {
		t.Fatalf("disabled %v", top)
	}
	for _, m := range []string{"solar-pro3", "solar-pro", "solar-open2", "solar-mini4", "solar-mini4-preview"} {
		if _, top := p.BuildAPIKwargsExtras(nil, map[string]any{"model": m}); top["reasoning_effort"] != "medium" {
			t.Fatalf("%s default %v", m, top)
		}
	}
	for _, m := range []string{"solar-mini", "solar-mini-250422", "solar-mini-202610", "solar-mini@q4", "syn-pro"} {
		extra, top := p.BuildAPIKwargsExtras(effortCfg("high"), map[string]any{"model": m})
		assertExtras(t, m, extra, top, map[string]any{}, map[string]any{})
	}
	if _, top := p.BuildAPIKwargsExtras(nil, map[string]any{}); top["reasoning_effort"] != "medium" {
		t.Fatalf("none model %v", top)
	}
}

func TestNebiusTokenFactoryProfile(t *testing.T) {
	h := newTestHome(t)
	t.Setenv("NEBIUS_API_KEY", "nebius-test-key")
	env := testEnv(t, h)
	for _, alias := range []string{"nebius", "nebius-tokenfactory", "nebius-tf", "token-factory", "tokenfactory"} {
		if got, err := resolveProvider(env, alias); err != nil || got != "nebius-token-factory" {
			t.Fatalf("resolveProvider(%s) = %q, %v", alias, got, err)
		}
		if got := NormalizeProvider(alias); got != "nebius-token-factory" {
			t.Fatalf("NormalizeProvider(%s) = %q", alias, got)
		}
	}
	p := bundledPluginProfile(t, "nebius-token-factory")
	extra, top := p.BuildAPIKwargsExtras(effortCfg("xhigh"), map[string]any{"model": "openai/gpt-oss-120b-fast"})
	assertExtras(t, "gpt-oss", extra, top, map[string]any{}, map[string]any{"reasoning_effort": "high"})
	for effort, want := range map[string]string{"ultra": "high", "max": "high", "xhigh": "high", "minimal": "low"} {
		if _, top := p.BuildAPIKwargsExtras(effortCfg(effort), map[string]any{"model": "deepseek-ai/DeepSeek-V4-Pro"}); top["reasoning_effort"] != want {
			t.Fatalf("%s -> %v", effort, top)
		}
	}
	if _, top := p.BuildAPIKwargsExtras(nil, map[string]any{"model": "deepseek-ai/DeepSeek-V4-Pro"}); top["reasoning_effort"] != "medium" {
		t.Fatalf("default %v", top)
	}
	extra, top = p.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(false), Effort: "high"}, map[string]any{"model": "deepseek-ai/DeepSeek-V4-Pro"})
	assertExtras(t, "disabled", extra, top, map[string]any{}, map[string]any{})
	extra, top = p.BuildAPIKwargsExtras(effortCfg("high"), map[string]any{"model": "meta-llama/Llama-3.3-70B-Instruct"})
	assertExtras(t, "llama", extra, top, map[string]any{}, map[string]any{})
	if _, top := p.BuildAPIKwargsExtras(effortCfg("low"), map[string]any{"model": "meta-llama/Llama-3.3-70B-Instruct", "supports_reasoning": true}); top["reasoning_effort"] != "low" {
		t.Fatalf("capable model %v", top)
	}
	model := "Qwen/Qwen3.5-397B-A17B-fast"
	for _, c := range [][3]string{{"nebius-token-factory/" + model, "nebius-token-factory", model}, {"nebius/" + model, "nebius-token-factory", model},
		{"nebius/" + model, "nebius", model}, {"openai/gpt-oss-120b-fast", "nebius-token-factory", "openai/gpt-oss-120b-fast"}} {
		if got := NormalizeModelForProvider(c[0], c[1]); got != c[2] {
			t.Fatalf("NormalizeModelForProvider(%s, %s) = %q", c[0], c[1], got)
		}
	}
}

func TestFireworksProfile(t *testing.T) {
	p := bundledPluginProfile(t, "fireworks")
	h := p.DefaultHeaders()
	if h["HTTP-Referer"] != attributionReferer || h["X-Title"] != attributionTitle || h["User-Agent"] != UserAgent("IcebathAgent") {
		t.Fatalf("headers %v", h)
	}
	for _, m := range append([]string{p.DefaultAuxModel}, p.FallbackModels...) {
		if !strings.HasPrefix(m, "accounts/fireworks/models/") || strings.Contains(m, "/routers/") || strings.Contains(strings.ToLower(m), "turbo") {
			t.Fatalf("not a pay as you go model %s", m)
		}
	}
	if len(p.FallbackModels) == 0 {
		t.Fatal("expected curated fallbacks")
	}
	extra, top := p.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(false)}, map[string]any{})
	assertExtras(t, "off", extra, top, map[string]any{}, map[string]any{"reasoning_effort": "none"})
	extra, top = p.BuildAPIKwargsExtras(effortCfg("low"), map[string]any{})
	assertExtras(t, "low", extra, top, map[string]any{}, map[string]any{"reasoning_effort": "low"})
	extra, top = p.BuildAPIKwargsExtras(nil, map[string]any{})
	assertExtras(t, "unset", extra, top, map[string]any{}, map[string]any{})
}

func TestGeminiAndVertexThinkingConfig(t *testing.T) {
	p := bundledPluginProfile(t, "gemini")
	if !containsStr(StaticCatalog(context.Background(), "gemini"), p.DefaultAuxModel) {
		t.Fatalf("aux model %s missing from the curated catalog", p.DefaultAuxModel)
	}
	r := effortCfg("high")
	raw := buildGeminiThinkingConfig("gemini-3.1-pro", hookReasoningCfg(r))
	if len(raw) == 0 {
		t.Fatal("expected a thinking config for gemini-3.1-pro")
	}
	native := p.BuildExtraBody("", map[string]any{"model": "gemini-3.1-pro", "reasoning_config": r})
	if !reflect.DeepEqual(native["thinking_config"], raw) {
		t.Fatalf("native %v", native)
	}
	compat := p.BuildExtraBody("", map[string]any{"model": "gemini-3.1-pro", "reasoning_config": r,
		"base_url": "https://generativelanguage.googleapis.com/v1beta/openai/"})
	want := map[string]any{"extra_body": map[string]any{"google": map[string]any{"thinking_config": snakeCaseGeminiThinkingConfig(raw)}}}
	if !reflect.DeepEqual(compat, want) {
		t.Fatalf("compat %v", compat)
	}
	if got := bundledPluginProfile(t, "vertex").BuildExtraBody("", map[string]any{"model": "gemini-3.1-pro", "reasoning_config": r}); !reflect.DeepEqual(got, want) {
		t.Fatalf("vertex %v", got)
	}
	if got := p.BuildExtraBody("", map[string]any{"model": "gpt-5"}); len(got) != 0 {
		t.Fatalf("no thinking config %v", got)
	}
}

func TestOllamaCloudReasoning(t *testing.T) {
	p := bundledPluginProfile(t, "ollama-cloud")
	run := func(r *Reasoning, supports bool) (map[string]any, map[string]any) {
		return p.BuildAPIKwargsExtras(r, map[string]any{"supports_reasoning": supports})
	}
	for _, e := range []string{"xhigh", "max", "MAX", "  Max  "} {
		extra, top := run(effortCfg(e), true)
		assertExtras(t, e, extra, top, map[string]any{}, map[string]any{"reasoning_effort": "max"})
	}
	for _, e := range []string{"low", "medium", "high"} {
		_, top := run(effortCfg(e), true)
		assertExtras(t, e, map[string]any{}, top, map[string]any{}, map[string]any{"reasoning_effort": e})
	}
	cases := []struct {
		r    *Reasoning
		want map[string]any
	}{
		{&Reasoning{Enabled: bptr(false)}, map[string]any{"reasoning_effort": "none"}},
		{&Reasoning{Enabled: bptr(false), Effort: "high"}, map[string]any{"reasoning_effort": "none"}},
		{effortCfg("none"), map[string]any{"reasoning_effort": "none"}},
		{nil, map[string]any{}},
		{effortCfg(""), map[string]any{}},
		{effortCfg("future-tier"), map[string]any{}},
		{effortCfg("minimal"), map[string]any{"reasoning_effort": "low"}},
	}
	for _, c := range cases {
		extra, top := run(c.r, true)
		assertExtras(t, "ollama", extra, top, map[string]any{}, c.want)
	}
	extra, top := run(effortCfg("xhigh"), false)
	assertExtras(t, "non thinking", extra, top, map[string]any{}, map[string]any{})
}

func TestOllamaModelSupportsThinkingProbe(t *testing.T) {
	orig := ollamaThinkingTransport
	t.Cleanup(func() { ollamaThinkingTransport = orig })
	respond := func(status int, body string, err error) {
		ollamaThinkingTransport = anthFuncTransport(func(r *http.Request) (*http.Response, error) {
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
		})
	}
	respond(200, `{"capabilities":["completion","tools","thinking"]}`, nil)
	if ok, known := OllamaModelSupportsThinking(context.Background(), "deepseek-v4-pro", "https://ollama.com/v1", "key", 0); !ok || !known {
		t.Fatalf("thinking capability (%v, %v)", ok, known)
	}
	respond(404, `{}`, nil)
	if _, known := OllamaModelSupportsThinking(context.Background(), "x", "https://ollama.com/v1", "key", 0); known {
		t.Fatal("probe failure must be unknown")
	}
	respond(0, "", errors.New("boom"))
	if _, known := OllamaModelSupportsThinking(context.Background(), "x", "https://ollama.com/v1", "key", 0); known {
		t.Fatal("transport error must be unknown")
	}
}

func TestTokenHubLMStudioAIGatewayNvidia(t *testing.T) {
	hub := bundledPluginProfile(t, "tencent-tokenhub")
	if d := hub.DefaultReasoningConfig("hy-3"); d == nil || d.Enabled == nil || !*d.Enabled || d.Effort != "high" {
		t.Fatalf("tokenhub default %+v", d)
	}
	for _, c := range []struct {
		r    *Reasoning
		want map[string]any
	}{{nil, map[string]any{}}, {&Reasoning{Enabled: bptr(false)}, map[string]any{}}, {&Reasoning{Enabled: bptr(true)}, map[string]any{"reasoning_effort": "high"}},
		{effortCfg("xhigh"), map[string]any{"reasoning_effort": "high"}}, {effortCfg("minimal"), map[string]any{"reasoning_effort": "low"}}} {
		extra, top := hub.BuildAPIKwargsExtras(c.r, map[string]any{})
		assertExtras(t, "tokenhub", extra, top, map[string]any{}, c.want)
	}
	lm := bundledPluginProfile(t, "lmstudio")
	extra, top := lm.BuildAPIKwargsExtras(effortCfg("high"), map[string]any{})
	assertExtras(t, "lmstudio aux", extra, top, genericNestedReasoning(hookReasoningCfg(effortCfg("high"))), map[string]any{})
	extra, top = lm.BuildAPIKwargsExtras(effortCfg("high"), map[string]any{"lmstudio_reasoning_options": []string{"off", "low", "high"}})
	assertExtras(t, "lmstudio unsupported", extra, top, map[string]any{}, map[string]any{})
	want, _ := resolveLMStudioEffort(hookReasoningCfg(effortCfg("high")), []string{"off", "low", "high"})
	extra, top = lm.BuildAPIKwargsExtras(effortCfg("high"), map[string]any{"lmstudio_reasoning_options": []any{"off", "low", "high"}, "supports_reasoning": true})
	assertExtras(t, "lmstudio probed", extra, top, map[string]any{}, map[string]any{"reasoning_effort": want})
	gw := bundledPluginProfile(t, "ai-gateway")
	extra, top = gw.BuildAPIKwargsExtras(nil, map[string]any{})
	assertExtras(t, "gateway unset", extra, top, map[string]any{"reasoning": map[string]any{"enabled": true, "effort": "medium"}}, map[string]any{})
	extra, top = gw.BuildAPIKwargsExtras(effortCfg("low"), map[string]any{"supports_reasoning": false})
	assertExtras(t, "gateway off", extra, top, map[string]any{}, map[string]any{})
	if h := gw.DefaultHeaders(); h["HTTP-Referer"] != attributionReferer || h["X-Title"] != attributionTitle {
		t.Fatalf("gateway headers %v", h)
	}
	nv := bundledPluginProfile(t, "nvidia")
	clean := []map[string]any{{"role": "user", "content": "hi"}, {"role": "tool", "content": "x"}}
	if got := nv.PrepareMessages(clean); &got[0] != &clean[0] {
		t.Fatal("untouched input must come back as is")
	}
	big := map[string]any{"role": "assistant", "content": "keep"}
	msgs := []map[string]any{big, {"role": "tool", "content": "out", "name": "f", "tool_name": "f", "tool_call_id": "1"}}
	got := nv.PrepareMessages(msgs)
	if !reflect.DeepEqual(got[1], map[string]any{"role": "tool", "content": "out", "tool_call_id": "1"}) || msgs[1]["name"] != "f" {
		t.Fatalf("stripped %v, original %v", got[1], msgs[1])
	}
	got[0]["probe"] = true
	if big["probe"] != true {
		t.Fatal("untouched messages must not be copied")
	}
}

func TestThinkingToggleParity(t *testing.T) {
	matrix := []*Reasoning{nil, {Enabled: bptr(false)}, {Enabled: bptr(true)}}
	for _, e := range []string{"low", "medium", "high", "xhigh", "max", "none"} {
		matrix = append(matrix, effortCfg(e))
	}
	for _, r := range matrix {
		for _, c := range [][2]string{{"kimi-coding", "kimi-k3"}, {"opencode-go", "kimi-k2.6"}, {"opencode-go", "deepseek-v4-pro"}} {
			extra, top := extrasOf(t, c[0], r, map[string]any{"model": c[1]})
			if _, th := extra["thinking"]; th {
				if _, ef := top["reasoning_effort"]; ef {
					t.Fatalf("%v both toggles on for %v", c, r)
				}
			}
		}
		extra, top := extrasOf(t, "deepseek", r, map[string]any{"model": "deepseek-v4-pro"})
		th, _ := extra["thinking"].(map[string]any)
		if th["type"] != "enabled" && th["type"] != "disabled" {
			t.Fatalf("deepseek thinking %v", extra)
		}
		effort, ok := top["reasoning_effort"].(string)
		if !ok {
			effort = deepseekV4Efforts[0]
		}
		if !containsStr(deepseekV4Efforts, effort) {
			t.Fatalf("deepseek effort %v", top)
		}
		if r != nil && r.Enabled != nil && !*r.Enabled {
			assertExtras(t, "deepseek off", extra, top, map[string]any{"thinking": map[string]any{"type": "disabled"}}, map[string]any{})
		}
		extra, top = extrasOf(t, "opencode-zen", r, map[string]any{"model": "x-preview-f-free"})
		if _, th := extra["thinking"]; th {
			t.Fatalf("ox alpha thinking %v", extra)
		}
		if e, ok := top["reasoning_effort"]; ok && e != "low" && e != "high" && e != "max" {
			t.Fatalf("ox alpha effort %v for %v", e, r)
		}
	}
}
