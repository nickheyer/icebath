package provider

import (
	"context"
	"strings"
	"testing"
)

var kimiThinkingEnabled = map[string]any{"thinking": map[string]any{"type": "enabled"}}

func TestKimiReasoningWireShapeNoConfigEnablesThinkingWithoutEffort(t *testing.T) {
	p := bundledPluginProfile(t, "kimi-coding")
	eb, tl := p.BuildAPIKwargsExtras(nil, map[string]any{})
	assertExtras(t, "nil", eb, tl, kimiThinkingEnabled, map[string]any{})
}

func TestKimiReasoningWireShapeEffortMappedToK3Vocabulary(t *testing.T) {
	p := bundledPluginProfile(t, "kimi-coding")
	for effort, want := range map[string]string{"low": "low", "minimal": "low", "medium": "high", "high": "high", "xhigh": "max", "max": "max", "ultra": "max"} {
		eb, tl := p.BuildAPIKwargsExtras(effortCfg(effort), map[string]any{})
		assertExtras(t, effort, eb, tl, map[string]any{}, map[string]any{"reasoning_effort": want})
	}
}

func TestKimiReasoningWireShapeEnabledWithoutEffortFallsBackToThinking(t *testing.T) {
	p := bundledPluginProfile(t, "kimi-coding")
	eb, tl := p.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(true)}, map[string]any{})
	assertExtras(t, "enabled", eb, tl, kimiThinkingEnabled, map[string]any{})
}

func TestKimiReasoningWireShapeUnrecognizedEffortFallsBackToThinking(t *testing.T) {
	p := bundledPluginProfile(t, "kimi-coding")
	for _, effort := range []string{"", "garbage"} {
		eb, tl := p.BuildAPIKwargsExtras(effortCfg(effort), map[string]any{})
		assertExtras(t, effort, eb, tl, kimiThinkingEnabled, map[string]any{})
	}
}

func TestKimiReasoningWireShapeDisabledSendsThinkingDisabledOnly(t *testing.T) {
	p := bundledPluginProfile(t, "kimi-coding")
	eb, tl := p.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(false)}, map[string]any{})
	assertExtras(t, "disabled", eb, tl, map[string]any{"thinking": map[string]any{"type": "disabled"}}, map[string]any{})
}

func TestKimiReasoningWireShapeNeverEmitsBoth(t *testing.T) {
	p := bundledPluginProfile(t, "kimi-coding")
	for _, r := range []*Reasoning{nil, {Enabled: bptr(true)}, effortCfg("high"), effortCfg("garbage"), {Enabled: bptr(false)}, {Enabled: bptr(false), Effort: "low"}} {
		eb, tl := p.BuildAPIKwargsExtras(r, map[string]any{})
		if _, both := eb["thinking"]; both && tl["reasoning_effort"] != nil {
			t.Fatalf("both emitted for %#v", r)
		}
	}
}

// K2 models keep the thinking state across turns and still honour a disable
func TestKimiK2ModelsKeepThinkingState(t *testing.T) {
	p := bundledPluginProfile(t, "kimi-coding")
	eb, tl := p.BuildAPIKwargsExtras(nil, map[string]any{"model": "kimi-K2.6"})
	assertExtras(t, "k2 keep", eb, tl, map[string]any{"thinking": map[string]any{"type": "enabled", "keep": "all"}}, map[string]any{})
	eb, _ = p.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(false)}, map[string]any{"model": "kimi-k2.6"})
	assertExtras(t, "k2 disabled", eb, map[string]any{}, map[string]any{"thinking": map[string]any{"type": "disabled"}}, map[string]any{})
}

func TestMoonshotProfilesExcludeBrotli(t *testing.T) {
	for _, name := range []string{"kimi-coding", "kimi-coding-cn"} {
		enc := ""
		for k, v := range bundledPluginProfile(t, name).DefaultHeaders() {
			if strings.ToLower(k) == "accept-encoding" {
				enc = strings.ToLower(v)
			}
		}
		if strings.Contains(enc, "br") || !strings.Contains(enc, "gzip") {
			t.Fatalf("%s accept-encoding %q", name, enc)
		}
	}
}

func TestMinimaxAuxModelM3ConsumerApiReturnsNonEmptyForEachProvider(t *testing.T) {
	for _, id := range []string{"minimax", "minimax-cn", "minimax-oauth"} {
		p := bundledPluginProfile(t, id)
		resolved := auxGetAuxModelForProvider(context.Background(), id)
		if resolved == "" || resolved != p.DefaultAuxModel {
			t.Fatalf("%s aux model %q, profile advertises %q", id, resolved, p.DefaultAuxModel)
		}
	}
}

func TestMinimaxM3OpenAIReasoningWireShapeM3OpenaiRouteRequestsReasoningSplitByDefault(t *testing.T) {
	p := bundledPluginProfile(t, "minimax")
	eb, tl := p.BuildAPIKwargsExtras(nil, map[string]any{"model": "MiniMax-M3", "base_url": "https://api.minimax.io/v1"})
	assertExtras(t, "m3 global", eb, tl, map[string]any{"reasoning_split": true}, map[string]any{})
}

func TestMinimaxM3OpenAIReasoningWireShapeNonM3OrNonGlobalOpenaiRoutesEmitNoOpenaiReasoningKnobs(t *testing.T) {
	p := bundledPluginProfile(t, "minimax")
	for _, c := range [][2]string{
		{"MiniMax-M2.7", "https://api.minimax.io/v1"},
		{"MiniMax-M3", "https://api.minimax.io/anthropic"},
		{"MiniMax-M3", "https://api.minimaxi.com/v1"},
	} {
		eb, tl := p.BuildAPIKwargsExtras(effortCfg("high"), map[string]any{"model": c[0], "base_url": c[1]})
		assertExtras(t, c[0]+" "+c[1], eb, tl, map[string]any{}, map[string]any{})
	}
}

func TestMinimaxM3OpenAIReasoningWireShapeTransportThreadsBaseUrlToProfile(t *testing.T) {
	p := bundledPluginProfile(t, "minimax")
	kwargs, err := chatCompletionsBuildKwargs("MiniMax-M3", []map[string]any{{"role": "user", "content": "ping"}}, nil, map[string]any{
		"provider_profile": p, "reasoning_config": map[string]any{"enabled": true, "effort": "medium"}, "base_url": "https://api.minimax.io/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertExtras(t, "transport", kwargs["extra_body"].(map[string]any), map[string]any{},
		map[string]any{"reasoning_split": true, "thinking": map[string]any{"type": "adaptive"}}, map[string]any{})
}

func TestMinimaxOauthAliasesEachDocumentedOauthAliasResolvesToMinimaxOauth(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, alias := range []string{"minimax_oauth", "minimax-portal", "minimax-global"} {
		p, ok := GetProviderProfile(context.Background(), alias)
		if !ok || p.Name != "minimax-oauth" {
			t.Fatalf("%s resolved to %q (%v)", alias, p.Name, ok)
		}
	}
}

func TestUpstageProfileFallbackModelsAreAgenticProOnly(t *testing.T) {
	p := bundledPluginProfile(t, "upstage")
	if len(p.FallbackModels) == 0 {
		t.Fatal("no fallback models")
	}
	for _, denied := range []string{"solar-mini", "syn-pro"} {
		for _, m := range p.FallbackModels {
			if strings.Contains(m, denied) {
				t.Fatalf("non-agentic family %q is a fallback default: %v", denied, p.FallbackModels)
			}
		}
	}
}

func TestUpstageReasoningProExplicitEffortPassesThrough(t *testing.T) {
	p := bundledPluginProfile(t, "upstage")
	for _, effort := range []string{"low", "medium", "high"} {
		eb, tl := p.BuildAPIKwargsExtras(effortCfg(effort), map[string]any{"model": "solar-pro3"})
		assertExtras(t, effort, eb, tl, map[string]any{}, map[string]any{"reasoning_effort": effort})
	}
}

func TestUpstageReasoningUnknownFutureEffortCollapsesToHigh(t *testing.T) {
	p := bundledPluginProfile(t, "upstage")
	_, tl := p.BuildAPIKwargsExtras(effortCfg("hyperthink"), map[string]any{"model": "solar-pro3"})
	assertExtras(t, "hyperthink", map[string]any{}, tl, map[string]any{}, map[string]any{"reasoning_effort": "high"})
}

func TestUpstageReasoningDisabledOmitsField(t *testing.T) {
	p := bundledPluginProfile(t, "upstage")
	_, tl := p.BuildAPIKwargsExtras(&Reasoning{Enabled: bptr(false), Effort: "high"}, map[string]any{"model": "solar-pro3"})
	assertExtras(t, "disabled", map[string]any{}, tl, map[string]any{}, map[string]any{})
}

func TestUpstageReasoningNoConfigDefaultsReasoningOn(t *testing.T) {
	p := bundledPluginProfile(t, "upstage")
	for _, model := range []string{"solar-pro3", "solar-pro", "solar-open2", "solar-mini4", "solar-mini4-preview"} {
		_, tl := p.BuildAPIKwargsExtras(nil, map[string]any{"model": model})
		assertExtras(t, model, map[string]any{}, tl, map[string]any{}, map[string]any{"reasoning_effort": "medium"})
	}
}

func TestUpstageReasoningDenyListedModelsNeverSendReasoning(t *testing.T) {
	p := bundledPluginProfile(t, "upstage")
	for _, model := range []string{"solar-mini", "solar-mini-250422", "solar-mini-202610", "solar-mini@q4", "syn-pro"} {
		eb, tl := p.BuildAPIKwargsExtras(effortCfg("high"), map[string]any{"model": model})
		assertExtras(t, model, eb, tl, map[string]any{}, map[string]any{})
	}
}

func TestUpstageReasoningNoneModelDefaultsToReasoning(t *testing.T) {
	p := bundledPluginProfile(t, "upstage")
	_, tl := p.BuildAPIKwargsExtras(nil, map[string]any{})
	assertExtras(t, "none model", map[string]any{}, tl, map[string]any{}, map[string]any{"reasoning_effort": "medium"})
}
