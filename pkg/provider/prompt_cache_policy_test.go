package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every cache_control marker reachable in a value
func collectCacheControls(v any) []map[string]any {
	var out []map[string]any
	switch t := v.(type) {
	case map[string]any:
		if m, ok := t["cache_control"]; ok {
			cm, _ := m.(map[string]any)
			out = append(out, cm)
		}
		for _, x := range t {
			out = append(out, collectCacheControls(x)...)
		}
	case []map[string]any:
		for _, x := range t {
			out = append(out, collectCacheControls(x)...)
		}
	case []any:
		for _, x := range t {
			out = append(out, collectCacheControls(x)...)
		}
	}
	return out
}

func cacheTestHome(t *testing.T, cacheTTL any) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeConfig(t, h, map[string]any{"prompt_caching": map[string]any{"cache_ttl": cacheTTL}})
}

func TestPlanCacheSectionsThreadsTTLAndPrefix(t *testing.T) {
	ctx := context.Background()
	off := false
	sys := []map[string]any{{"role": "system", "content": "You are helpful."}, {"role": "user", "content": "hello"}}
	out, _ := planCacheSectionsForDestination(ctx, sys, nil, "anthropic", "https://api.anthropic.com", "anthropic_messages", "claude-opus-4.8", &off, "1h", nil)
	markers := collectCacheControls(out)
	if len(markers) == 0 {
		t.Fatal("expected cache_control markers on a caching route")
	}
	for _, m := range markers {
		if m["ttl"] != "1h" {
			t.Fatalf("the configured 1h tier must reach the destination plan markers: %v", markers)
		}
	}
	split := []map[string]any{{"role": "system", "content": "stable prefix\nvolatile suffix"}, {"role": "user", "content": "hello"}}
	prefix := "stable prefix"
	out, _ = PlanCacheSectionsForDestination(ctx, split, nil, "anthropic", "https://api.anthropic.com", "anthropic_messages", "claude-opus-4.8", &off, "5m", &prefix)
	parts, ok := out[0]["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("the destination system prompt must split into static and volatile parts: %#v", out[0]["content"])
	}
	if p0, _ := parts[0].(map[string]any); p0["text"] != "stable prefix" {
		t.Fatalf("static part %v", parts[0])
	}
	if p1, _ := parts[1].(map[string]any); p1["text"] != "\nvolatile suffix" {
		t.Fatalf("volatile part %v", parts[1])
	}
	if split[0]["content"] != "stable prefix\nvolatile suffix" {
		t.Fatal("caller messages must not be mutated")
	}
	out, _ = planCacheSectionsForDestination(ctx, sys, nil, "opencode", "https://api.opencode.ai", "chat_completions", "qwen3.6-plus", &off, "1h", nil)
	markers = collectCacheControls(out)
	if len(markers) == 0 {
		t.Fatal("opencode+qwen is a cache-honoring route")
	}
	for _, m := range markers {
		if _, has := m["ttl"]; has {
			t.Fatalf("Qwen's 5-minute-only context cache must clamp a configured 1h: %v", markers)
		}
	}
}

func TestPlanCacheSectionsStripsOnNonCachingRoute(t *testing.T) {
	cacheTestHome(t, "off")
	ctx := context.Background()
	msgs := []map[string]any{{"role": "system", "content": "s", "cache_control": map[string]any{"type": "ephemeral"}}, {"role": "user", "content": "q"}}
	tools := []map[string]any{{"type": "function", "cache_control": map[string]any{"type": "ephemeral"}}}
	out, outTools := planCacheSectionsForDestination(ctx, msgs, tools, "anthropic", "https://api.anthropic.com", "anthropic_messages", "claude-opus-4.8", nil, "1h", nil)
	if len(collectCacheControls(out)) != 0 || len(collectCacheControls(outTools)) != 0 {
		t.Fatalf("a configured disable must strip markers: %v %v", out, outTools)
	}
	if _, has := msgs[0]["cache_control"]; !has {
		t.Fatal("caller messages must keep their markers")
	}
	if !BlankCachePolicyAgent(ctx, nil).CacheDisabled || !PromptCachingDisabledFromConfig(ctx) {
		t.Fatal("blank policy agent must read the configured disable")
	}
	on := false
	if BlankCachePolicyAgent(ctx, &on).CacheDisabled {
		t.Fatal("explicit cache_disabled false wins over config")
	}
}

func TestConfiguredCacheTTLReadsValidTiers(t *testing.T) {
	for _, tier := range []string{"1h", "5m"} {
		cacheTestHome(t, tier)
		if got := ConfiguredCacheTTL(context.Background()); got != tier {
			t.Fatalf("tier %s got %q", tier, got)
		}
	}
}

func TestConfiguredCacheTTLNoneForDisabledOrUnknown(t *testing.T) {
	for _, value := range []any{"off", false, nil, "2h"} {
		cacheTestHome(t, value)
		if got := configuredCacheTTL(context.Background()); got != "" {
			t.Fatalf("%v gave %q", value, got)
		}
	}
}

func TestCacheTTLMeansDisabled(t *testing.T) {
	for v, want := range map[any]bool{"5m": false, "1h": false, "off": true, "OFF": true, "none": true, "no": true, "disabled": true, "2h": false, 0: false, true: false} {
		if CacheTTLMeansDisabled(v) != want {
			t.Fatalf("%v want %v", v, want)
		}
	}
	if !CacheTTLMeansDisabled(nil) || !CacheTTLMeansDisabled(false) {
		t.Fatal("None and False disable caching")
	}
}

func TestAuxReplanThreadsConfiguredTTLToMarkers(t *testing.T) {
	cacheTestHome(t, "1h")
	dest := auxFallbackDestination{Provider: "anthropic", BaseURL: "https://api.anthropic.com", APIMode: "anthropic_messages", Model: "claude-opus-4.8"}
	msgs := []map[string]any{{"role": "system", "content": "You are helpful."}, {"role": "user", "content": "hello"},
		{"role": "assistant", "content": "hi"}, {"role": "user", "content": "next"}}
	out, _ := auxReplanSynchronousCacheSections(context.Background(), msgs, nil, dest)
	markers := collectCacheControls(out)
	if len(markers) == 0 {
		t.Fatal("expected cache_control markers on a caching route")
	}
	for _, m := range markers {
		if m["ttl"] != "1h" {
			t.Fatalf("the configured 1h tier must reach auxiliary fallback replans: %v", markers)
		}
	}
}

func TestAuxReplanLeavesOneShotUnmarkedOnCachingRoute(t *testing.T) {
	cacheTestHome(t, "1h")
	ctx := context.Background()
	msgs := []map[string]any{{"role": "system", "content": "Summarize the transcript."}, {"role": "user", "content": "TRANSCRIPT"}}
	anth := auxFallbackDestination{Provider: "anthropic", BaseURL: "https://api.anthropic.com", APIMode: "anthropic_messages", Model: "claude-opus-4.8"}
	for _, dest := range []auxFallbackDestination{anth,
		{Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIMode: "chat_completions", Model: "anthropic/claude-opus-4.8"}} {
		out, _ := auxReplanSynchronousCacheSections(ctx, msgs, nil, dest)
		if m := collectCacheControls(out); len(m) != 0 {
			t.Fatalf("%s: one shot must stay unmarked %v", dest.Provider, m)
		}
	}
	advisor := applyAnthropicCacheControl(msgs, "1h", true, "", true)
	out, _ := auxReplanSynchronousCacheSections(ctx, advisor, nil, anth)
	if len(collectCacheControls(out)) == 0 {
		t.Fatal("a pre-decorated advisor request must keep caching")
	}
}

func TestAnthropicPromptCachePolicyBlankCachePolicyAgentBranches(t *testing.T) {
	ctx := context.Background()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeConfig(t, h, map[string]any{})
	s := func(v string) *string { return &v }
	cases := []struct {
		provider, base, mode, model string
		cache, native               bool
	}{
		{"anthropic", "https://api.anthropic.com", "anthropic_messages", "claude-opus-4.8", true, true},
		{"openrouter", "https://openrouter.ai/api/v1", "chat_completions", "anthropic/claude-opus-4.8", true, false},
		{"openrouter", "https://openrouter.ai/api/v1", "chat_completions", "moonshotai/kimi-k2.6", true, false},
		{"nous", "https://inference-api.nousresearch.com/v1", "chat_completions", "qwen3.6-plus", true, false},
		{"minimax", "https://api.minimax.io/anthropic", "anthropic_messages", "MiniMax-M3", false, false},
		{"minimax", "https://api.minimax.io/anthropic", "anthropic_messages", "MiniMax-M2.7", true, true},
		{"opencode-go", "https://opencode.ai/zen/go/v1", "chat_completions", "qwen3.6-plus", true, false},
		{"opencode-go", "https://opencode.ai/zen/go/v1", "chat_completions", "deepseek-v4", false, false},
		{"openai", "https://api.openai.com/v1", "chat_completions", "gpt-5.4", false, false},
	}
	for _, c := range cases {
		off := false
		cache, native := ResolveAnthropicPromptCachePolicy(ctx, BlankCachePolicyAgent(ctx, &off),
			CacheDestination{Provider: s(c.provider), BaseURL: s(c.base), APIMode: s(c.mode), Model: s(c.model)})
		if cache != c.cache || native != c.native {
			t.Fatalf("%s %s got (%v, %v) want (%v, %v)", c.provider, c.model, cache, native, c.cache, c.native)
		}
	}
	agent := &CachePolicyAgent{Provider: "custom:lab", BaseURL: "https://lab.example/v1", APIMode: "chat_completions", Model: map[string]any{"default": "lab-model"},
		CustomProviders: []map[string]any{{"name": "lab", "base_url": "https://lab.example/v1", "models": map[string]any{"lab-model": map[string]any{"prompt_caching": true}}}}}
	if cache, native := ResolveAnthropicPromptCachePolicy(ctx, agent, CacheDestination{}); !cache || native {
		t.Fatalf("declared custom prompt_caching on the OpenAI wire gives envelope layout, got (%v, %v)", cache, native)
	}
	agent.CacheDisabled = true
	if cache, _ := ResolveAnthropicPromptCachePolicy(ctx, agent, CacheDestination{}); cache {
		t.Fatal("operator disable wins over every branch")
	}
}

func TestProviderRequestHeadersOverrideClientHeaders(t *testing.T) {
	seen := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "c1", "object": "chat.completion", "model": "m",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}})
	}))
	defer srv.Close()
	client, err := newChatCompletionsClient(Route{Provider: "custom", BaseURL: srv.URL + "/v1", APIKey: "k", Mode: ModeChatCompletions, Model: "m",
		Headers: map[string]string{"X-Route": "client", "X-Keep": "kept"}})
	if err != nil {
		t.Fatal(err)
	}
	aux := auxProviderClientAux(context.Background(), client)
	resp, err := aux.CreateChatCompletion(context.Background(), map[string]any{"model": "m",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "extra_headers": map[string]any{"X-Route": "call", "X-Extra": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	got := <-seen
	if got.Get("X-Route") != "call" || got.Get("X-Extra") != "1" || got.Get("X-Keep") != "kept" {
		t.Fatalf("per request headers must win over the client's own: %v", got)
	}
	if ExtractContentOrReasoning(resp, -1) != "ok" {
		t.Fatalf("response %v", resp)
	}
	ctx := withProviderRequestHeaders(context.Background(), map[string]string{"X-Call": "v", "Authorization": "Bearer call"})
	cx, err := newCodexResponsesClient(Route{Provider: "openai-codex", BaseURL: srv.URL, APIKey: "k", Mode: ModeCodexResponses, Model: "gpt-5.4"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := cx.(*codexResponsesClient).newRequest(ctx, &codexPrepared{url: srv.URL, headers: map[string]string{"Authorization": "Bearer own"}}, []byte("{}"))
	if err != nil || req.Header.Get("X-Call") != "v" || req.Header.Get("Authorization") != "Bearer call" {
		t.Fatalf("codex request headers %v %v", req, err)
	}
	an, err := newAnthropicClient(Route{Provider: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "sk-ant-api03-x", Mode: ModeAnthropicMessages, Model: "claude-opus-4.8",
		TokenSource: func(context.Context) (string, error) { return "sk-ant-api03-x", nil }})
	if err != nil {
		t.Fatal(err)
	}
	headers, _, err := an.(*anthropicClient).requestHeaders(ctx, nil, nil)
	if err != nil || headers["X-Call"] != "v" || headers["Authorization"] != "Bearer call" {
		t.Fatalf("anthropic request headers %v %v", headers, err)
	}
	if providerRequestHeaders(context.Background()) != nil {
		t.Fatal("a call without extra headers binds none")
	}
}
