package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Registered profile for name, failing the test when absent
func registeredProfile(t *testing.T, name string) Profile {
	t.Helper()
	p, ok := GetProviderProfile(context.Background(), name)
	if !ok {
		t.Fatalf("%s provider profile must be registered", name)
	}
	return p
}

func thinkingBody(kind string) map[string]any {
	return map[string]any{"thinking": map[string]any{"type": kind}}
}

func TestDeepSeekRegisteredProfileThinkingWireShape(t *testing.T) {
	p := registeredProfile(t, "deepseek")
	pro := map[string]any{"model": "deepseek-v4-pro"}
	eb, tl := p.BuildAPIKwargsExtras(nil, pro)
	assertExtras(t, "v4 pro default", eb, tl, thinkingBody("enabled"), map[string]any{})
	for _, effort := range []string{"low", "medium", "high"} {
		_, tl = p.BuildAPIKwargsExtras(effortCfg(effort), pro)
		assertExtras(t, effort, map[string]any{}, tl, map[string]any{}, map[string]any{"reasoning_effort": effort})
	}
	for _, effort := range []string{"xhigh", "max", "MAX", "  Max  "} {
		_, tl = p.BuildAPIKwargsExtras(effortCfg(effort), pro)
		assertExtras(t, effort, map[string]any{}, tl, map[string]any{}, map[string]any{"reasoning_effort": "max"})
	}
	eb, tl = p.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(false)}, pro)
	assertExtras(t, "disabled", eb, tl, thinkingBody("disabled"), map[string]any{})
	flash := map[string]any{"model": "deepseek-v4.1-flash-expires-on-0910"}
	for _, cfg := range []*Reasoning{{Effort: "none"}, {Effort: "false"}, {Effort: "disabled"}, {Effort: "NONE"}, {Enabled: bptr(true), Effort: "none"}} {
		eb, tl = p.BuildAPIKwargsExtras(cfg, flash)
		assertExtras(t, "effort "+cfg.Effort, eb, tl, thinkingBody("disabled"), map[string]any{})
	}
	_, tl = p.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(false), Effort: "high"}, pro)
	assertExtras(t, "disabled with effort", map[string]any{}, tl, map[string]any{}, map[string]any{})
	for _, effort := range []string{"garbage", ""} {
		_, tl = p.BuildAPIKwargsExtras(effortCfg(effort), pro)
		assertExtras(t, "effort "+effort, map[string]any{}, tl, map[string]any{}, map[string]any{})
	}
}

func TestDeepSeekRegisteredProfileModelGating(t *testing.T) {
	p := registeredProfile(t, "deepseek")
	for _, model := range []string{"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v4-future-variant", "DEEPSEEK-V4-PRO", "deepseek-flash", "DEEPSEEK-FLASH"} {
		eb, _ := p.BuildAPIKwargsExtras(nil, map[string]any{"model": model})
		assertExtras(t, model, eb, map[string]any{}, thinkingBody("enabled"), map[string]any{})
	}
	for _, model := range []any{"deepseek-v3-0324", "deepseek-v3.1", "", nil, "deepseek-unknown"} {
		eb, tl := p.BuildAPIKwargsExtras(effortCfg("high"), map[string]any{"model": model})
		assertExtras(t, "non thinking", eb, tl, map[string]any{}, map[string]any{})
	}
}

// Chat completions kwargs for a deepseek request on the public endpoint
func deepseekKwargs(t *testing.T, model string, reasoning any) map[string]any {
	t.Helper()
	return ccTransportKwargs(t, model, nil, map[string]any{"provider_profile": registeredProfile(t, "deepseek"),
		"reasoning_config": reasoning, "base_url": "https://api.deepseek.com/v1", "provider_name": "deepseek"})
}

func TestDeepSeekFullKwargsIntegration(t *testing.T) {
	kw := deepseekKwargs(t, "deepseek-v4-pro", map[string]any{"enabled": true, "effort": "high"})
	if kw["model"] != "deepseek-v4-pro" || kw["reasoning_effort"] != "high" || !reflect.DeepEqual(kw["extra_body"], thinkingBody("enabled")) {
		t.Fatalf("v4 kwargs %v", kw)
	}
	kw = deepseekKwargs(t, "deepseek-v3-0324", map[string]any{"enabled": true, "effort": "high"})
	if _, ok := kw["reasoning_effort"]; ok {
		t.Fatalf("v3 kwargs %v", kw)
	}
	if eb, ok := kw["extra_body"].(map[string]any); ok {
		if _, has := eb["thinking"]; has {
			t.Fatalf("v3 thinking %v", kw)
		}
	}
	for _, tc := range []struct {
		model     string
		reasoning any
		want      string
	}{
		{"deepseek-v4.1-flash-expires-on-0910", nil, "enabled"},
		{"deepseek-v4.1-flash-expires-on-0910", map[string]any{"effort": "none"}, "disabled"},
		{"deepseek-v4-pro", ParseReasoningEffort("none"), "disabled"},
	} {
		kw = deepseekKwargs(t, tc.model, tc.reasoning)
		if _, ok := kw["reasoning_effort"]; ok || !reflect.DeepEqual(kw["extra_body"], thinkingBody(tc.want)) {
			t.Fatalf("%s %v kwargs %v", tc.model, tc.reasoning, kw)
		}
	}
}

func TestDeepSeekAuxModel(t *testing.T) {
	p := registeredProfile(t, "deepseek")
	for _, m := range p.FallbackModels {
		if normalizeForDeepseek(m) != m {
			t.Fatalf("fallback %q is not a current id", m)
		}
	}
	if p.DefaultAuxModel == "" || auxGetAuxModelForProvider(context.Background(), "deepseek") != p.DefaultAuxModel {
		t.Fatalf("aux model %q", p.DefaultAuxModel)
	}
}

func TestFireworksHeadersAttributionMatchesCanonicalHermesValues(t *testing.T) {
	p := registeredProfile(t, "fireworks")
	headers, base := p.DefaultHeaders(), auxORHeadersBase()
	if headers["HTTP-Referer"] != base["HTTP-Referer"] || headers["X-Title"] != base["X-Title"] {
		t.Fatalf("headers %v base %v", headers, base)
	}
}

func TestFireworksHeadersUserAgentIdentifiesHermes(t *testing.T) {
	p := registeredProfile(t, "fireworks")
	if headers := p.DefaultHeaders(); headers["User-Agent"] != UserAgent("IcebathAgent") {
		t.Fatalf("user agent %q", headers["User-Agent"])
	}
}

// Fails unless the model is a direct pay as you go catalog id on the standard tier
func auxRequirePaygFireworksModel(t *testing.T, m string) {
	t.Helper()
	if !strings.HasPrefix(m, "accounts/fireworks/models/") || strings.Contains(m, "/routers/") || strings.Contains(strings.ToLower(m), "turbo") {
		t.Fatalf("not a pay as you go model %q", m)
	}
}

func TestFireworksModelDefaultsAuxModelIsPaygModelNotRouter(t *testing.T) {
	auxRequirePaygFireworksModel(t, registeredProfile(t, "fireworks").DefaultAuxModel)
}

func TestFireworksModelDefaultsFallbackModelsArePaygModelsNotRouters(t *testing.T) {
	p := registeredProfile(t, "fireworks")
	if len(p.FallbackModels) == 0 {
		t.Fatal("expected curated fallbacks")
	}
	for _, m := range p.FallbackModels {
		auxRequirePaygFireworksModel(t, m)
	}
}

func TestMinimaxAuxModelM3ConsumerAPIMatchesEveryProfile(t *testing.T) {
	for _, id := range []string{"minimax", "minimax-cn", "minimax-oauth"} {
		p := registeredProfile(t, id)
		if got := auxGetAuxModelForProvider(context.Background(), id); got == "" || got != p.DefaultAuxModel {
			t.Fatalf("%s aux model %q profile %q", id, got, p.DefaultAuxModel)
		}
	}
}

func TestMinimaxM3OpenAIReasoningWireShape(t *testing.T) {
	p := registeredProfile(t, "minimax")
	eb, tl := p.BuildAPIKwargsExtras(nil, map[string]any{"model": "MiniMax-M3", "base_url": "https://api.minimax.io/v1"})
	assertExtras(t, "m3 global", eb, tl, map[string]any{"reasoning_split": true}, map[string]any{})
	for _, tc := range [][2]string{{"MiniMax-M2.7", "https://api.minimax.io/v1"}, {"MiniMax-M3", "https://api.minimax.io/anthropic"},
		{"MiniMax-M3", "https://api.minimaxi.com/v1"}} {
		eb, tl = p.BuildAPIKwargsExtras(effortCfg("high"), map[string]any{"model": tc[0], "base_url": tc[1]})
		assertExtras(t, tc[0]+" "+tc[1], eb, tl, map[string]any{}, map[string]any{})
	}
	kw := ccTransportKwargs(t, "MiniMax-M3", nil, map[string]any{"provider_profile": p,
		"reasoning_config": map[string]any{"enabled": true, "effort": "medium"}, "base_url": "https://api.minimax.io/v1"})
	if want := map[string]any{"reasoning_split": true, "thinking": map[string]any{"type": "adaptive"}}; !reflect.DeepEqual(kw["extra_body"], want) {
		t.Fatalf("transport extra body %v", kw["extra_body"])
	}
}

func TestMinimaxOAuthDocumentedAliasesResolve(t *testing.T) {
	for _, alias := range []string{"minimax_oauth", "minimax-portal", "minimax-global"} {
		if p, ok := GetProviderProfile(context.Background(), alias); !ok || p.Name != "minimax-oauth" {
			t.Fatalf("%s resolved to %q %v", alias, p.Name, ok)
		}
	}
}
