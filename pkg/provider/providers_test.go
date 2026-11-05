package provider

import (
	"context"
	"reflect"
	"testing"
)

// Replaces the models.dev provider lookup with a fixed table, a missing id answering nil
func fakeModelsDevInfo(t *testing.T, table map[string]*ModelsDevProvider) {
	t.Helper()
	prev := modelsDevInfoFn
	modelsDevInfoFn = func(_ context.Context, canonical string, _ bool) *ModelsDevProvider { return table[canonical] }
	t.Cleanup(func() { modelsDevInfoFn = prev })
}

func TestProviderOverlayDefinesTheModelsDevKeyedProvider(t *testing.T) {
	ctx := contextWithTempHome(t)
	fakeModelsDevInfo(t, nil)
	registerTestProfile(t, Profile{Name: "zz-acp", Aliases: []string{"zz-acp-agent", "zz-acp-cli"}, DisplayName: "ZZ ACP",
		BaseURL: "acp://zz", AuthType: "external_process", OverlayAliases: []string{"zz-acp-agent"},
		Overlay: &IcebathOverlay{Transport: "codex_responses", AuthType: "external_process", BaseURLOverride: "acp://zz",
			BaseURLEnvVar: "ZZ_ACP_BASE_URL"}})
	registerTestProfile(t, Profile{Name: "zz-plain", Aliases: []string{"zzp"}, BaseURL: "https://zz.test/v1"})
	if got := NormalizeProviderDef(ctx, "ZZ-ACP-Agent"); got != "zz-acp" {
		t.Fatalf("overlay alias = %q", got)
	}
	if got := NormalizeProviderDef(ctx, "zzp"); got != "zzp" {
		t.Fatalf("no overlay alias = %q", got)
	}
	if got := NormalizeProviderDef(ctx, "zz-acp-cli"); got != "zz-acp-cli" {
		t.Fatalf("plain alias = %q", got)
	}
	pdef := GetProviderDef(ctx, "zz-acp-agent", true)
	if pdef == nil || pdef.ID != "zz-acp" || pdef.Name != "ZZ ACP" || pdef.Transport != "codex_responses" ||
		pdef.BaseURL != "acp://zz" || pdef.BaseURLEnv != "ZZ_ACP_BASE_URL" || pdef.Source != "icebath" {
		t.Fatalf("pdef = %+v", pdef)
	}
	if got := determineModeCtx(ctx, "zz-acp", "", "", ""); got != ModeCodexResponses {
		t.Fatalf("mode = %q", got)
	}
	overlays := IcebathOverlays(ctx)
	if _, ok := overlays["zz-acp"]; !ok {
		t.Fatal("zz-acp overlay missing")
	}
	if _, ok := overlays["zz-plain"]; ok {
		t.Fatal("zz-plain has no overlay")
	}
}

func TestMirroredAliasKeysKeepTheOverlayRoute(t *testing.T) {
	ctx := contextWithTempHome(t)
	fakeModelsDevInfo(t, nil)
	registerTestProfile(t, Profile{Name: "zz-acp", Aliases: []string{"zz-acp-agent"}, DisplayName: "ZZ ACP",
		BaseURL: "acp://zz", OverlayAliases: []string{"zz-acp-agent"},
		Overlay: &IcebathOverlay{Transport: "codex_responses", BaseURLOverride: "acp://zz"}})
	pdef := ResolveProviderFull(ctx, "zz-acp-agent", nil, nil)
	if pdef == nil || pdef.ID != "zz-acp" || pdef.Transport != "codex_responses" || pdef.Source != "icebath" {
		t.Fatalf("pdef = %+v", pdef)
	}
}

func TestModelsDevKeyedOverlayRowKeepsTheProfileLabelOffline(t *testing.T) {
	ctx := contextWithTempHome(t)
	fakeModelsDevInfo(t, nil)
	registerTestProfile(t, Profile{Name: "zz-hosted", Aliases: []string{"zz-hosted-alias"}, DisplayName: "ZZ Hosted",
		ModelsDevID: "zz-catalog", OverlayAliases: []string{"zzh"},
		Overlay: &IcebathOverlay{ExtraEnvVars: []string{"ZZ_HOSTED_TOKEN"}}})
	if a, b := GetProviderLabel(ctx, "zz-hosted"), GetProviderLabel(ctx, "zzh"); a != "ZZ Hosted" || b != "ZZ Hosted" {
		t.Fatalf("labels = %q %q", a, b)
	}
}

func TestProvidersNormalizeAliases(t *testing.T) {
	ctx := contextWithTempHome(t)
	cases := map[string]string{
		"chatgpt": "openai-codex", "chatgpt-codex": "openai-codex", "gmi-cloud": "gmi", "gmicloud": "gmi",
		"fireworks": "fireworks", "fireworks-ai": "fireworks", "fw": "fireworks", "tencent": "tencent-tokenhub",
		"tokenhub": "tencent-tokenhub", "tencent-cloud": "tencent-tokenhub", "tencentmaas": "tencent-tokenhub",
		"aci": "actual", "ollama": "custom", "ollama-cloud": "ollama-cloud",
	}
	for alias, want := range cases {
		if got := NormalizeProviderDef(ctx, alias); got != want {
			t.Errorf("NormalizeProviderDef(%q) = %q, want %q", alias, got, want)
		}
	}
}

func TestProvidersDetermineAPIModeOverlays(t *testing.T) {
	ctx := contextWithTempHome(t)
	fakeModelsDevInfo(t, nil)
	cases := []struct {
		provider, baseURL string
		want              APIMode
	}{
		{"actual", "https://api.actual.inc", ModeChatCompletions},
		{"tencent", "", ModeChatCompletions},
		{"minimax", "", ModeAnthropicMessages},
	}
	for _, c := range cases {
		if got := determineModeCtx(ctx, c.provider, c.baseURL, "", ""); got != c.want {
			t.Errorf("determine(%q, %q) = %q, want %q", c.provider, c.baseURL, got, c.want)
		}
	}
}

func TestVertexRegisteredInIcebathOverlays(t *testing.T) {
	ctx := contextWithTempHome(t)
	fakeModelsDevInfo(t, nil)
	if pdef := GetProviderDef(ctx, "vertex", true); pdef == nil || pdef.AuthType != "vertex" {
		t.Fatalf("vertex = %+v", pdef)
	}
}

func TestCommandCodeResolveProviderFull(t *testing.T) {
	ctx := contextWithTempHome(t)
	fakeModelsDevInfo(t, nil)
	chat := ResolveProviderFull(ctx, "commandcode", map[string]any{}, []any{})
	if chat == nil || chat.ID != "commandcode" || chat.Transport != "openai_chat" || !reflect.DeepEqual(chat.KeyEnv, []string{"COMMANDCODE_API_KEY"}) {
		t.Fatalf("commandcode = %+v", chat)
	}
	anth := ResolveProviderFull(ctx, "commandcode-anthropic", map[string]any{}, []any{})
	if anth == nil || anth.ID != "commandcode-anthropic" {
		t.Fatalf("commandcode-anthropic = %+v", anth)
	}
}

func TestGetProviderMergesModelsDevWithTheOverlay(t *testing.T) {
	ctx := contextWithTempHome(t)
	fakeModelsDevInfo(t, map[string]*ModelsDevProvider{
		"zai":     {ID: "zai", Name: "Z.AI", Env: []string{"ZHIPU_API_KEY", "ZAI_API_KEY"}, API: "https://api.z.ai/api/paas/v4", Doc: "https://docs.z.ai"},
		"vercel":  {ID: "vercel", Name: "Vercel AI Gateway", Env: []string{"AI_GATEWAY_API_KEY"}, API: "https://ai-gateway.vercel.sh/v1"},
		"stepfun": {ID: "stepfun", Name: "StepFun", Env: []string{"STEPFUN_API_KEY"}, API: "https://api.stepfun.com/v1"},
		"zz-mdev": {ID: "zz-mdev", Name: "ZZ Catalog", Env: []string{"ZZ_KEY"}, API: "https://zz.example/v1"},
	})
	zai := GetProviderDef(ctx, "glm", true)
	if zai == nil || zai.ID != "zai" || zai.Name != "Z.AI" || zai.Source != "models.dev" || zai.Doc != "https://docs.z.ai" ||
		zai.BaseURL != "https://api.z.ai/api/paas/v4" || zai.BaseURLEnv != "GLM_BASE_URL" ||
		!reflect.DeepEqual(zai.KeyEnv, []string{"ZHIPU_API_KEY", "ZAI_API_KEY", "GLM_API_KEY", "Z_AI_API_KEY"}) {
		t.Fatalf("zai = %+v", zai)
	}
	if step := GetProviderDef(ctx, "step", true); step == nil || step.BaseURL != "https://api.stepfun.ai/step_plan/v1" {
		t.Fatalf("stepfun override = %+v", step)
	}
	if !isAggregatorCtx(ctx, "ai-gateway") || !isRoutingAggregatorCtx(ctx, "ai-gateway") {
		t.Fatal("vercel is a routing aggregator")
	}
	if !isAggregatorCtx(ctx, "opencode-go") || isRoutingAggregatorCtx(ctx, "opencode-go") || isRoutingAggregatorCtx(ctx, "zen") {
		t.Fatal("flat namespace resellers are aggregators but never routing aggregators")
	}
	if !isAggregatorCtx(ctx, "custom:my-proxy") || isAggregatorCtx(ctx, "zai") {
		t.Fatal("custom slugs aggregate, zai does not")
	}
	if got := GetProviderLabel(ctx, "openai-codex"); got != "ChatGPT or Codex Subscription" {
		t.Fatalf("label override = %q", got)
	}
	if got := GetProviderLabel(ctx, "zai"); got != "Z.AI" {
		t.Fatalf("models.dev label = %q", got)
	}
	if got := GetProviderLabel(ctx, "nowhere-provider"); got != "nowhere-provider" {
		t.Fatalf("unknown label = %q", got)
	}
	direct := ResolveProviderFull(ctx, "zz-mdev", nil, nil)
	if direct == nil || direct.ID != "zz-mdev" || direct.Source != "models.dev" || direct.BaseURL != "https://zz.example/v1" {
		t.Fatalf("models.dev rung = %+v", direct)
	}
}

func TestResolveProviderFullRungOrder(t *testing.T) {
	ctx := contextWithTempHome(t)
	fakeModelsDevInfo(t, nil)
	user := map[string]any{
		"openai":  map[string]any{"api": "https://api.openai.com/v1", "key_env": "OPENAI_API_KEY", "name": "OpenAI Direct"},
		"bedrock": map[string]any{"stale_timeout_seconds": 600},
		"my-gw":   map[string]any{"url": "https://gw.example/v1", "api_key_env": "GW_KEY", "transport": "anthropic_messages"},
	}
	openai := ResolveProviderFull(ctx, "OpenAI", user, nil)
	if openai == nil || openai.ID != "openai" || openai.Name != "OpenAI Direct" || openai.Source != "user-config" ||
		!reflect.DeepEqual(openai.KeyEnv, []string{"OPENAI_API_KEY"}) {
		t.Fatalf("raw user provider must win over the openrouter alias: %+v", openai)
	}
	if bedrock := ResolveProviderFull(ctx, "bedrock", user, nil); bedrock == nil || bedrock.Source == "user-config" || bedrock.Transport != "bedrock_converse" {
		t.Fatalf("a tuning only block must not shadow the built in: %+v", bedrock)
	}
	if gw := ResolveProviderFull(ctx, "my-gw", user, nil); gw == nil || gw.Transport != "anthropic_messages" || gw.BaseURL != "https://gw.example/v1" {
		t.Fatalf("user provider rung = %+v", gw)
	}
	custom := []any{
		"not a dict",
		map[string]any{"name": "No URL"},
		map[string]any{"name": "Local Box", "base_url": "http://box:8000/v1", "key_env": "BOX_KEY"},
		map[string]any{"name": "Keyed", "url": "http://keyed/v1", "provider_key": "kp"},
	}
	if box := ResolveProviderFull(ctx, "custom:local-box", nil, custom); box == nil || box.ID != "custom:local-box" || box.Name != "Local Box" ||
		!reflect.DeepEqual(box.KeyEnv, []string{"BOX_KEY"}) {
		t.Fatalf("custom provider rung = %+v", box)
	}
	if keyed := ResolveProviderFull(ctx, "kp", nil, custom); keyed == nil || keyed.ID != "custom:kp" {
		t.Fatalf("keyed custom provider = %+v", keyed)
	}
	if healed := ResolveProviderFull(ctx, "custom", nil, custom); healed == nil || healed.ID != "custom:local-box" {
		t.Fatalf("bare custom heals onto the first entry: %+v", healed)
	}
	if ResolveProviderFull(ctx, "nowhere-provider", user, custom) != nil {
		t.Fatal("unknown provider resolved")
	}
}

func TestLossyAliasCollapseKeepsTheExactRegistryID(t *testing.T) {
	ctx := contextWithTempHome(t)
	fakeModelsDevInfo(t, nil)
	cn := ResolveProviderFull(ctx, "kimi-coding-cn", nil, nil)
	if cn == nil || cn.ID != "kimi-coding-cn" || cn.Source != "icebath-auth-registry" || cn.Transport != "openai_chat" {
		t.Fatalf("kimi-coding-cn = %+v", cn)
	}
	if kimi := ResolveProviderFull(ctx, "kimi-for-coding", nil, nil); kimi == nil || kimi.ID != "kimi-for-coding" || kimi.Source != "icebath" {
		t.Fatalf("models.dev id keeps the overlay chain: %+v", kimi)
	}
}

func TestActualRouteAndHostMandateUseExactHostnames(t *testing.T) {
	ctx := contextWithTempHome(t)
	if !isActualRouteCtx(ctx, "custom", "https://api.actual.inc/v1") || !isActualRouteCtx(ctx, "aci", "") {
		t.Fatal("actual host or alias not recognised")
	}
	for _, u := range []string{"https://api.actual.inc.example/v1", "https://proxy.example/api.actual.inc/v1"} {
		if isActualRouteCtx(ctx, "custom", u) {
			t.Errorf("lookalike %q treated as actual", u)
		}
	}
	for _, u := range []string{"https://generic.example.com/v1", "https://api.openai.com.attacker.test/v1", "https://proxy.test/api.openai.com/v1"} {
		if got := HostMandatedMode(u); got != "" {
			t.Errorf("HostMandatedMode(%q) = %q", u, got)
		}
	}
	if HostMandatedMode("https://api.openai.com/v1") != ModeCodexResponses || HostMandatedMode("https://api.anthropic.com") != ModeAnthropicMessages {
		t.Fatal("real endpoints must be mandated")
	}
}
