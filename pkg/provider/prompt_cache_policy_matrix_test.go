package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Policy agent of a built in route with the attached empty custom provider list
func pcpAgent(provider, baseURL, apiMode, model string) *CachePolicyAgent {
	return &CachePolicyAgent{Provider: provider, BaseURL: baseURL, APIMode: apiMode, Model: model, CustomProviders: []map[string]any{}}
}

// Custom provider list with one entry declaring the model capabilities
func pcpCustom(name, baseURL, model string, caps map[string]any) []map[string]any {
	return []map[string]any{{"name": name, "base_url": baseURL, "models": map[string]any{model: caps}}}
}

func pcpStr(v string) *string { return &v }

// Temporary empty home so no case reads the developer config
func pcpHome(t *testing.T) context.Context {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return context.Background()
}

func pcpExpect(t *testing.T, ctx context.Context, agent *CachePolicyAgent, dest CacheDestination, should, native bool) {
	t.Helper()
	gotShould, gotNative := ResolveAnthropicPromptCachePolicy(ctx, agent, dest)
	if gotShould != should || gotNative != native {
		t.Fatalf("policy (%v, %v), want (%v, %v)", gotShould, gotNative, should, native)
	}
}

const (
	pcpOR       = "https://openrouter.ai/api/v1"
	pcpCC       = "chat_completions"
	pcpAM       = "anthropic_messages"
	pcpGateway  = "https://gateway.example.com/anthropic"
	pcpEdge     = "https://models.example.net/v1"
	pcpLitellm  = "https://litellm.internal.example.com/v1"
	pcpNous     = "https://inference-api.nousresearch.com/v1"
	pcpOpencode = "https://opencode.ai/v1"
)

func TestAnthropicPromptCachePolicyMatrix(t *testing.T) {
	type pcpCase struct {
		name           string
		agent          *CachePolicyAgent
		dest           CacheDestination
		should, native bool
	}
	withCustom := func(a *CachePolicyAgent, cps []map[string]any) *CachePolicyAgent {
		a.CustomProviders = cps
		return a
	}
	disabled := func(a *CachePolicyAgent) *CachePolicyAgent {
		a.CacheDisabled = true
		return a
	}
	edge := func(enabled bool, apiMode string) *CachePolicyAgent {
		return withCustom(pcpAgent("custom:edge-router", pcpEdge, apiMode, "vendor-agnostic-model"),
			pcpCustom("edge-router", pcpEdge, "vendor-agnostic-model", map[string]any{"prompt_caching": enabled}))
	}
	spelled := edge(true, pcpCC)
	spelled.Provider = "unrelated-runtime-alias"
	spelled.CustomProviders[0]["base_url"] = "HTTPS://models.example.net/v1/"
	undeclared := edge(true, pcpCC)
	undeclared.CustomProviders[0]["models"] = map[string]any{"vendor-agnostic-model": map[string]any{"context_length": 131072}}
	drift := edge(true, pcpCC)
	drift.Provider = "some-unrelated-alias"
	drift.BaseURL = "https://Models.Example.NET/v1/"
	cases := []pcpCase{
		{"test_claude_on_native_anthropic_caches_with_native_layout", pcpAgent("anthropic", "https://api.anthropic.com", pcpAM, "claude-sonnet-4-6"), CacheDestination{}, true, true},
		{"test_anthropic_provider_on_third_party_host_stays_message_only", pcpAgent("anthropic", "https://api.minimax.io/anthropic", pcpAM, "claude-sonnet-4-6"), CacheDestination{}, true, true},
		{"test_claude_on_openrouter_caches_with_envelope_layout", pcpAgent("openrouter", pcpOR, pcpCC, "anthropic/claude-sonnet-4.6"), CacheDestination{}, true, false},
		{"test_non_claude_on_openrouter_does_not_cache", pcpAgent("openrouter", pcpOR, pcpCC, "openai/gpt-5.4"), CacheDestination{}, false, false},
		{"test_kimi_k26_on_openrouter_caches_with_envelope_layout", pcpAgent("openrouter", pcpOR, pcpCC, "moonshotai/kimi-k2.6"), CacheDestination{}, true, false},
		{"test_kimi_bare_release_slug_on_openrouter_caches", pcpAgent("openrouter", pcpOR, pcpCC, "k2-thinking"), CacheDestination{}, true, false},
		{"test_kimi_on_non_openrouter_host_does_not_cache", pcpAgent("custom", "https://api.moonshot.cn/v1", pcpCC, "moonshotai/kimi-k2.6"), CacheDestination{}, false, false},
		{"test_minimax_claude_via_anthropic_messages", pcpAgent("custom", "https://api.minimax.io/anthropic", pcpAM, "claude-sonnet-4-6"), CacheDestination{}, true, true},
		{"test_third_party_anthropic_non_claude_unknown_provider_does_not_cache", pcpAgent("custom", "https://some-unknown-gateway.example.com/anthropic", pcpAM, "glm-4.5"), CacheDestination{}, false, false},
		{"test_bare_alias_with_explicit_prompt_caching_capability_caches", withCustom(pcpAgent("custom:anthropic-proxy", pcpGateway, pcpAM, "fable"), pcpCustom("anthropic-proxy", pcpGateway, "fable", map[string]any{"prompt_caching": true})), CacheDestination{}, true, true},
		{"test_explicit_prompt_caching_false_is_authoritative", withCustom(pcpAgent("custom:anthropic-proxy", pcpGateway, pcpAM, "claude-fable-5"), pcpCustom("anthropic-proxy", pcpGateway, "claude-fable-5", map[string]any{"prompt_caching": false})), CacheDestination{}, false, false},
		{"test_bare_alias_without_capability_stays_conservative", withCustom(pcpAgent("custom:anthropic-proxy", pcpGateway, pcpAM, "fable"), pcpCustom("anthropic-proxy", pcpGateway, "fable", map[string]any{"context_length": 1000000})), CacheDestination{}, false, false},
		{"test_capability_on_other_route_does_not_apply", withCustom(pcpAgent("custom:anthropic-proxy", pcpGateway, pcpAM, "fable"), pcpCustom("other-proxy", "https://other.example.com/anthropic", "fable", map[string]any{"prompt_caching": true})), CacheDestination{}, false, false},
		{"test_operator_cache_disable_beats_explicit_capability_true", disabled(withCustom(pcpAgent("custom:anthropic-proxy", pcpGateway, pcpAM, "fable"), pcpCustom("anthropic-proxy", pcpGateway, "fable", map[string]any{"prompt_caching": true}))), CacheDestination{}, false, false},
		{"test_explicit_true_enables_envelope_layout_on_chat_completions", edge(true, pcpCC), CacheDestination{}, true, false},
		{"test_explicit_false_disables_markers_on_chat_completions", edge(false, pcpCC), CacheDestination{}, false, false},
		{"test_equivalent_base_url_spelling_reaches_capability_lookup", spelled, CacheDestination{}, true, false},
		{"test_undeclared_chat_completions_route_stays_conservative", undeclared, CacheDestination{}, false, false},
		{"test_explicit_true_does_not_cross_into_other_wire_protocols[codex_responses]", edge(true, "codex_responses"), CacheDestination{}, false, false},
		{"test_explicit_true_does_not_cross_into_other_wire_protocols[bedrock_converse]", edge(true, "bedrock_converse"), CacheDestination{}, false, false},
		{"test_gate_matches_declaration_despite_url_spelling_drift", drift, CacheDestination{}, true, false},
		{"test_gate_matches_declaration_via_spaced_legacy_name", withCustom(pcpAgent("custom:my-gateway", "https://GW.example.net/v1/", pcpCC, "alias-model"), pcpCustom("My Gateway", "https://gw.example.net/v1", "alias-model", map[string]any{"prompt_caching": true})), CacheDestination{}, true, false},
		{"test_minimax_m27_on_provider_minimax_caches_native_layout", pcpAgent("minimax", "https://api.minimax.io/anthropic", pcpAM, "minimax-m2.7"), CacheDestination{}, true, true},
		{"test_custom_provider_pointed_at_minimax_host_caches", pcpAgent("custom", "https://api.minimax.io/anthropic", pcpAM, "minimax-m2.7"), CacheDestination{}, true, true},
		{"test_minimax_host_china_endpoint_caches", pcpAgent("custom", "https://api.minimaxi.com/anthropic", pcpAM, "minimax-m2.1"), CacheDestination{}, true, true},
		{"test_minimax_provider_on_openai_wire_does_not_cache", pcpAgent("minimax", "https://api.minimax.io/v1", pcpCC, "minimax-m2.7"), CacheDestination{}, false, false},
		{"test_minimax_m3_on_provider_minimax_does_not_cache", pcpAgent("minimax", "https://api.minimax.io/anthropic", pcpAM, "MiniMax-M3[1m]"), CacheDestination{}, false, false},
		{"test_minimax_m3_on_china_endpoint_does_not_cache", pcpAgent("minimax-cn", "https://api.minimaxi.com/anthropic", pcpAM, "MiniMax-M3"), CacheDestination{}, false, false},
		{"test_minimax_m3_via_custom_provider_does_not_cache", pcpAgent("custom", "https://api.minimaxi.com/anthropic", pcpAM, "MiniMax-M3[1m]"), CacheDestination{}, false, false},
		{"test_minimax_m3_via_provider_anthropic_proxy_does_not_cache", pcpAgent("anthropic", "https://api.minimax.io/anthropic", pcpAM, "MiniMax-M3"), CacheDestination{}, false, false},
		{"test_minimax_m27_via_provider_anthropic_proxy_still_caches", pcpAgent("anthropic", "https://api.minimax.io/anthropic", pcpAM, "MiniMax-M2.7"), CacheDestination{}, true, true},
		{"test_custom_openai_wire_does_not_cache_even_with_claude_name", pcpAgent("custom", "https://api.fireworks.ai/inference/v1", pcpCC, "claude-sonnet-4"), CacheDestination{}, false, false},
		{"test_qwen_on_opencode_go_caches_with_envelope_layout", pcpAgent("opencode-go", pcpOpencode, pcpCC, "qwen3.6-plus"), CacheDestination{}, true, false},
		{"test_qwen_on_opencode_zen_caches", pcpAgent("opencode", pcpOpencode, pcpCC, "qwen3-coder-plus"), CacheDestination{}, true, false},
		{"test_qwen_on_nous_portal_caches_with_envelope_layout", pcpAgent("nous", pcpNous, pcpCC, "qwen3.6-plus"), CacheDestination{}, true, false},
		{"test_non_qwen_non_claude_on_nous_portal_does_not_cache", pcpAgent("nous", pcpNous, pcpCC, "openai/gpt-5.4"), CacheDestination{}, false, false},
		{"test_deepseek_on_direct_alibaba_does_not_cache", pcpAgent("alibaba", "https://dashscope.aliyuncs.com/compatible-mode/v1", pcpCC, "deepseek-v4-pro"), CacheDestination{}, false, false},
		{"test_deepseek_on_openrouter_does_not_cache", pcpAgent("openrouter", pcpOR, pcpCC, "deepseek/deepseek-chat"), CacheDestination{}, false, false},
		{"test_litellm_claude_operator_disable_still_wins", disabled(pcpAgent("litellm", pcpLitellm, pcpCC, "claude-opus-4.8")), CacheDestination{}, false, false},
		{"test_litellm_in_anthropic_proxy_mode_still_uses_native_layout", pcpAgent("litellm", "https://litellm.internal.example.com", pcpAM, "claude-opus-4.8"), CacheDestination{}, true, true},
		{"test_operator_capability_declaration_overrides_litellm_inference", withCustom(pcpAgent("custom:litellm", pcpLitellm, pcpCC, "claude-opus-4.8"), pcpCustom("litellm", pcpLitellm, "claude-opus-4.8", map[string]any{"prompt_caching": false})), CacheDestination{}, false, false},
		{"test_capability_declared_true_keeps_envelope_layout_on_openai_wire", withCustom(pcpAgent("custom:litellm", pcpLitellm, pcpCC, "claude-opus-4.8"), pcpCustom("litellm", pcpLitellm, "claude-opus-4.8", map[string]any{"prompt_caching": true})), CacheDestination{}, true, false},
		{"test_capability_declared_false_wins_over_openrouter_grant", withCustom(pcpAgent("custom:litellm", pcpOR, pcpCC, "claude-opus-4.8"), pcpCustom("litellm", pcpOR, "claude-opus-4.8", map[string]any{"prompt_caching": false})), CacheDestination{}, false, false},
		{"test_litellm_provider_on_lookalike_host_still_grants", pcpAgent("custom:litellm", "https://notlitellm.attacker.example/v1", pcpCC, "claude-opus-4.8"), CacheDestination{}, true, false},
		{"test_portal_claude_on_the_messages_wire_uses_the_native_layout", pcpAgent("nous", pcpNous, pcpAM, "anthropic/claude-opus-4.8"), CacheDestination{}, true, true},
		{"test_portal_claude_on_chat_completions_keeps_the_envelope_layout", pcpAgent("nous", pcpNous, pcpCC, "anthropic/claude-opus-4.8"), CacheDestination{}, true, false},
		{"test_overrides_take_precedence_over_self", pcpAgent("openrouter", pcpOR, pcpCC, "openai/gpt-5.4"), CacheDestination{Model: pcpStr("anthropic/claude-sonnet-4.6")}, true, false},
		{"test_fallback_target_evaluated_independently", pcpAgent("anthropic", "https://api.anthropic.com", pcpAM, "claude-opus-4.6"), CacheDestination{Provider: pcpStr("openrouter"), BaseURL: pcpStr(pcpOR), APIMode: pcpStr(pcpCC), Model: pcpStr("anthropic/claude-sonnet-4.6")}, true, false},
	}
	for _, p := range []string{"opencode", "opencode-zen", "opencode-go"} {
		cases = append(cases, pcpCase{"test_deepseek_on_opencode_does_not_cache[" + p + "]", pcpAgent(p, pcpOpencode, pcpCC, "deepseek-v4-pro"), CacheDestination{}, false, false})
	}
	for _, pb := range [][2]string{{"litellm", "https://my-litellm-host.example.com/v1"}, {"custom:litellm", "https://my-litellm-host.example.com/v1"},
		{"custom", "https://litellm.internal.example.com/v1"}, {"custom", "https://my-litellm-gw.internal.example.com/v1"}} {
		cases = append(cases, pcpCase{"test_claude_on_litellm_openai_wire_caches_with_envelope_layout[" + pb[0] + "-" + pb[1] + "]", pcpAgent(pb[0], pb[1], pcpCC, "anthropic/claude-sonnet-4.6"), CacheDestination{}, true, false})
	}
	for _, m := range []string{"openai/gpt-5.4", "gemini-2.5-pro", "qwen3.6-plus", "deepseek-v4-pro"} {
		cases = append(cases, pcpCase{"test_non_claude_on_litellm_openai_wire_does_not_cache[" + m + "]", pcpAgent("litellm", pcpLitellm, pcpCC, m), CacheDestination{}, false, false})
	}
	for _, u := range []string{"https://notlitellm.attacker.example/v1", "https://foolitellmbar.example/v1", "https://gateway.attacker.example/litellm/v1"} {
		cases = append(cases, pcpCase{"test_litellm_lookalike_hosts_do_not_cache[" + u + "]", pcpAgent("custom", u, pcpCC, "claude-opus-4.8"), CacheDestination{}, false, false})
	}
	for _, p := range []string{"custom:notlitellm", "notlitellm", "mylitellmthing"} {
		cases = append(cases, pcpCase{"test_litellm_lookalike_provider_names_do_not_cache[" + p + "]", pcpAgent(p, "https://gateway.attacker.example/v1", pcpCC, "claude-opus-4.8"), CacheDestination{}, false, false})
	}
	for _, p := range []string{"litellm", "custom:litellm", "litellm-router", "LiteLLM"} {
		cases = append(cases, pcpCase{"test_litellm_provider_spellings_still_cache[" + p + "]", pcpAgent(p, "https://gateway.internal.example/v1", pcpCC, "claude-opus-4.8"), CacheDestination{}, true, false})
	}
	for _, m := range []string{"codex_responses", "bedrock_converse", "codex_app_server"} {
		cases = append(cases, pcpCase{"test_litellm_claude_on_other_transports_does_not_cache[" + m + "]", pcpAgent("custom:litellm", pcpLitellm, m, "claude-opus-4.8"), CacheDestination{}, false, false})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pcpExpect(t, pcpHome(t), c.agent, c.dest, c.should, c.native)
		})
	}
}

func TestAnthropicPromptCachePolicyToolMarkerCapability(t *testing.T) {
	t.Run("test_anthropic_provider_on_third_party_host_stays_message_only", func(t *testing.T) {
		if directNativeAnthropicToolCacheCapability(pcpAgent("anthropic", "https://api.minimax.io/anthropic", pcpAM, "claude-sonnet-4-6"), CacheDestination{}) {
			t.Fatal("a third party host must not get tool markers")
		}
	})
	t.Run("test_only_direct_native_anthropic_enables_tool_markers", func(t *testing.T) {
		agent := pcpAgent("anthropic", "https://api.anthropic.com", pcpAM, "claude-sonnet-4-6")
		if !directNativeAnthropicToolCacheCapability(agent, CacheDestination{}) {
			t.Fatal("direct native Anthropic must get tool markers")
		}
		if directNativeAnthropicToolCacheCapability(agent, CacheDestination{Provider: pcpStr("custom"), BaseURL: pcpStr("https://api.minimax.io/anthropic"), APIMode: pcpStr(pcpAM), Model: pcpStr("claude-sonnet-4-6")}) {
			t.Fatal("a custom Anthropic gateway must not get tool markers")
		}
		if directNativeAnthropicToolCacheCapability(agent, CacheDestination{Provider: pcpStr("openrouter"), BaseURL: pcpStr(pcpOR), APIMode: pcpStr(pcpCC), Model: pcpStr("anthropic/claude-sonnet-4.6")}) {
			t.Fatal("OpenRouter must not get tool markers")
		}
	})
}

func TestAnthropicPromptCachePolicyConfigAndProbes(t *testing.T) {
	t.Run("test_modern_providers_yaml_through_real_loader", func(t *testing.T) {
		ctx := pcpHome(t)
		pcpWriteYAML(t, "providers:\n  anthropic-proxy:\n    api: https://gateway.example.com/anthropic\n    transport: anthropic_messages\n    models:\n      fable:\n        context_length: 1000000\n        prompt_caching: true\n      opus:\n        prompt_caching: false\n")
		agent := pcpAgent("custom:anthropic-proxy", pcpGateway, pcpAM, "fable")
		agent.CustomProviders = nil
		pcpExpect(t, ctx, agent, CacheDestination{}, true, true)
		agent.Model = "opus"
		pcpExpect(t, ctx, agent, CacheDestination{}, false, false)
	})
	t.Run("test_modern_providers_yaml_is_honored_during_early_init", func(t *testing.T) {
		ctx := pcpHome(t)
		pcpWriteYAML(t, "providers:\n  edge-router:\n    api: https://models.example.net/v1\n    transport: openai_chat\n    models:\n      vendor-agnostic-model:\n        prompt_caching: true\n")
		agent := pcpAgent("edge-router", pcpEdge, pcpCC, "vendor-agnostic-model")
		agent.CustomProviders = nil
		pcpExpect(t, ctx, agent, CacheDestination{}, true, false)
	})
	t.Run("test_unrelated_custom_route_skips_capability_lookup", func(t *testing.T) {
		ctx := pcpHome(t)
		agent := pcpAgent("openrouter", pcpOR, pcpCC, "openai/gpt-5.4")
		agent.CustomProviders = pcpCustom("edge-router", pcpEdge, "vendor-agnostic-model", map[string]any{"prompt_caching": true})
		orig := cachePolicyCapabilityLookup
		t.Cleanup(func() { cachePolicyCapabilityLookup = orig })
		cachePolicyCapabilityLookup = func(string, string, string, []map[string]any, map[string]any) (bool, bool) {
			t.Fatal("unrelated built-in route performed custom capability lookup")
			return false, false
		}
		pcpExpect(t, ctx, agent, CacheDestination{}, false, false)
	})
	t.Run("test_early_init_probe_never_fetches_catalog_from_network", func(t *testing.T) {
		ctx := pcpHome(t)
		var seen []bool
		orig := cachePolicyProviderDef
		t.Cleanup(func() { cachePolicyProviderDef = orig })
		cachePolicyProviderDef = func(ctx context.Context, name string, allowNetwork bool) *ProviderDef {
			seen = append(seen, allowNetwork)
			return orig(ctx, name, allowNetwork)
		}
		agent := pcpAgent("openrouter", pcpOR, pcpCC, "anthropic/claude-sonnet-4.6")
		agent.CustomProviders = nil
		pcpExpect(t, ctx, agent, CacheDestination{}, true, false)
		if len(seen) == 0 {
			t.Fatal("None-branch did not consult the provider catalog")
		}
		for _, v := range seen {
			if v {
				t.Fatal("the probe must stay off the network")
			}
		}
	})
}

func pcpWriteYAML(t *testing.T, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAnthropicPromptCachePolicyPlannerMarkers(t *testing.T) {
	t.Run("test_litellm_openai_wire_emits_no_top_level_marker", func(t *testing.T) {
		ctx := pcpHome(t)
		messages := []map[string]any{
			{"role": "system", "content": strings.Repeat("SYSTEM ", 200)},
			{"role": "user", "content": "go"},
			{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c0", "type": "function", "function": map[string]any{"name": "terminal", "arguments": "{}"}}}},
			{"role": "tool", "tool_call_id": "c0", "content": strings.Repeat("output ", 100)},
			{"role": "assistant", "content": "done"},
		}
		off := false
		planned, _ := PlanCacheSectionsForDestination(ctx, messages, nil, "custom:litellm", pcpLitellm, pcpCC, "claude-opus-4.8", &off, "5m", nil)
		inner := 0
		for _, m := range planned {
			if _, has := m["cache_control"]; has {
				t.Fatalf("no breakpoint may sit on the message envelope on the OpenAI wire %v", m)
			}
			inner += len(collectCacheControls(m["content"]))
		}
		if inner == 0 {
			t.Fatal("the OpenAI-wire grant must still place real breakpoints")
		}
	})
	const deployed = "qwen3.7-plus"
	history := func() []map[string]any {
		return []map[string]any{{"role": "system", "content": "sys prompt"}, {"role": "user", "content": "first"}, {"role": "assistant", "content": "reply"}, {"role": "user", "content": "second"}}
	}
	t.Run("test_caching_remains_enabled_for_opencode_go_qwen", func(t *testing.T) {
		ctx := pcpHome(t)
		off := false
		agent := BlankCachePolicyAgent(ctx, &off)
		agent.CustomProviders = []map[string]any{}
		pcpExpect(t, ctx, agent, CacheDestination{Provider: pcpStr("opencode-go"), BaseURL: pcpStr(""), APIMode: pcpStr(pcpCC), Model: pcpStr(deployed)}, true, false)
		if effectiveCacheTTL("1h", deployed, "opencode-go") != "1h" {
			t.Fatal("opencode-go keeps the 1h tier")
		}
	})
	t.Run("test_destination_planner_emits_1h_marker_on_opencode_go", func(t *testing.T) {
		ctx := pcpHome(t)
		off := false
		msgs, tools := PlanCacheSectionsForDestination(ctx, history(), []map[string]any{}, "opencode-go", "", pcpCC, deployed, &off, "1h", nil)
		markers := append(collectCacheControls(msgs), collectCacheControls(tools)...)
		if len(markers) == 0 {
			t.Fatal("opencode-go/qwen must stay cache-eligible")
		}
		for _, m := range markers {
			if m["ttl"] != "1h" {
				t.Fatalf("markers %v", markers)
			}
		}
		msgs, tools = PlanCacheSectionsForDestination(ctx, history(), []map[string]any{}, "opencode", "", pcpCC, deployed, &off, "1h", nil)
		markers = append(collectCacheControls(msgs), collectCacheControls(tools)...)
		if len(markers) == 0 {
			t.Fatal("opencode/qwen stays cache-eligible")
		}
		for _, m := range markers {
			if _, has := m["ttl"]; has {
				t.Fatalf("markers %v", markers)
			}
		}
	})
}
