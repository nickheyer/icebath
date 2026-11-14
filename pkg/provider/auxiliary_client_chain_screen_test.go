package provider

import (
	"context"
	"strings"
	"testing"
)

// Fallback chain entry of one provider and model on the example endpoint
func auxChainEntry(provider, model string) map[string]any {
	return map[string]any{"provider": provider, "model": model, "base_url": "https://example.com/v1", "api_key": "k"}
}

// Home whose compression task carries the given fallback chain
func auxChainHome(t *testing.T, entries ...map[string]any) context.Context {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	chain := make([]any, 0, len(entries))
	for _, e := range entries {
		chain = append(chain, e)
	}
	writeConfig(t, h, map[string]any{"auxiliary": map[string]any{"compression": map[string]any{"fallback_chain": chain}}})
	return context.Background()
}

func TestConfiguredChainContextScreening(t *testing.T) {
	t.Run("test_configured_chain_skips_too_small_candidate_for_compression", func(t *testing.T) {
		ctx := auxChainHome(t, auxChainEntry("small-provider", "tiny-8k"), auxChainEntry("big-provider", "huge-1m"))
		small := &auxFakeResponsesClient{base: "small"}
		large := &auxFakeResponsesClient{base: "large"}
		origResolve, origCtx := auxResolveFallbackEntryFn, auxModelContextLengthFn
		t.Cleanup(func() { auxResolveFallbackEntryFn, auxModelContextLengthFn = origResolve, origCtx })
		auxResolveFallbackEntryFn = func(_ context.Context, entry map[string]any) (AuxClient, string, error) {
			if entry["provider"] == "small-provider" {
				return small, "tiny-8k", nil
			}
			return large, "huge-1m", nil
		}
		auxModelContextLengthFn = func(_ context.Context, req ContextLengthRequest) int {
			switch req.Model {
			case "tiny-8k":
				return 8192
			case "huge-1m":
				return 1048576
			}
			return 256000
		}
		client, model, label, err := auxTryConfiguredFallbackChain(ctx, "compression", "auto", "", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if client != AuxClient(large) {
			t.Fatalf("chain returned the first reachable candidate without screening by context window: %v", client)
		}
		if model != "huge-1m" || !strings.Contains(label, "big-provider") {
			t.Fatalf("model %q label %q", model, label)
		}
	})
	t.Run("test_same_provider_same_model_still_skipped", func(t *testing.T) {
		ctx := auxChainHome(t, auxChainEntry("nvidia", "deepseek-ai/deepseek-v4-pro"))
		orig := auxResolveFallbackEntryFn
		t.Cleanup(func() { auxResolveFallbackEntryFn = orig })
		auxResolveFallbackEntryFn = func(context.Context, map[string]any) (AuxClient, string, error) {
			t.Fatal("must not be resolved")
			return nil, "", nil
		}
		client, model, label, err := auxTryConfiguredFallbackChain(ctx, "compression", "nvidia", "", "deepseek-ai/deepseek-v4-pro", "", "")
		if err != nil || client != nil || model != "" || label != "" {
			t.Fatalf("client %v model %q label %q err %v", client, model, label, err)
		}
	})
	t.Run("test_compression_task_uses_minimum_context_constant", func(t *testing.T) {
		if n, ok := auxTaskMinimumContextLength("compression"); !ok || n != MinimumContextLength {
			t.Fatalf("compression minimum %d %v", n, ok)
		}
		for _, task := range []string{"vision", "title_generation", "web_extract", "skills_hub", "mcp", "session_search", ""} {
			if _, ok := auxTaskMinimumContextLength(task); ok {
				t.Fatalf("%q must carry no minimum", task)
			}
		}
	})
}

type auxResolvedCall struct {
	provider, model string
	opts            ProviderClientOptions
}

// Runs one configured moa_aggregator fallback entry through the sync candidate call with one tool
func auxRunConfiguredFallback(t *testing.T, entry map[string]any) (*auxCallFakeClient, []auxResolvedCall, []map[string]any) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeConfig(t, h, map[string]any{"auxiliary": map[string]any{"moa_aggregator": map[string]any{"fallback_chain": []any{entry}}}})
	ctx := context.Background()
	client := &auxCallFakeClient{base: auxStrOr(entry["base_url"]), create: func(context.Context, map[string]any) (any, error) { return auxCallOK("ok"), nil }}
	var calls []auxResolvedCall
	prev := auxResolveProviderClientFn
	t.Cleanup(func() { auxResolveProviderClientFn = prev })
	auxResolveProviderClientFn = func(_ context.Context, provider, model string, opts ProviderClientOptions) (AuxClient, string, error) {
		calls = append(calls, auxResolvedCall{provider, model, opts})
		return client, model, nil
	}
	fbClient, fbModel, label, err := auxTryConfiguredFallbackChain(ctx, "moa_aggregator", "primary", "", "", "", "")
	if err != nil || fbClient == nil {
		t.Fatalf("fallback client %v err %v", fbClient, err)
	}
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}
	if _, err := auxCallFallbackCandidate(ctx, fbClient, fbModel, label, auxFallbackRequest{Task: "moa_aggregator",
		Messages: []map[string]any{{"role": "system", "content": "stable prefix"}, {"role": "user", "content": "lookup"}},
		Tools:    tools, EffectiveTimeout: 30, EffectiveExtraBody: map[string]any{}}, false); err != nil {
		t.Fatal(err)
	}
	if len(client.sent) == 0 {
		t.Fatal("the fallback client was never called")
	}
	return client, calls, tools
}

func TestSynchronousFallbackCachePlans(t *testing.T) {
	t.Run("test_direct_anthropic_fallback_uses_entry_destination_for_tool_marker", func(t *testing.T) {
		client, calls, tools := auxRunConfiguredFallback(t, map[string]any{"provider": "anthropic", "model": "claude-sonnet-4-6",
			"base_url": "https://api.anthropic.com", "api_mode": "anthropic_messages"})
		if len(calls) != 1 || calls[0].provider != "anthropic" || calls[0].model != "claude-sonnet-4-6" ||
			calls[0].opts.ExplicitBaseURL != "https://api.anthropic.com" || calls[0].opts.ExplicitAPIKey != nil || calls[0].opts.APIMode != "anthropic_messages" {
			t.Fatalf("resolved calls %+v", calls)
		}
		wireTools := auxDictList(client.sent[len(client.sent)-1]["tools"])
		if _, has := wireTools[len(wireTools)-1]["cache_control"]; !has {
			t.Fatalf("direct native Anthropic tools carry the marker %v", wireTools)
		}
		if _, has := tools[len(tools)-1]["cache_control"]; has {
			t.Fatal("caller tools must stay unmarked")
		}
	})
	t.Run("test_third_party_anthropic_fallback_keeps_message_markers_without_tool_marker", func(t *testing.T) {
		client, calls, tools := auxRunConfiguredFallback(t, map[string]any{"provider": "custom", "model": "claude-sonnet-4-6",
			"base_url": "https://api.minimax.io/anthropic", "api_mode": "anthropic_messages"})
		if calls[0].opts.ExplicitBaseURL != "https://api.minimax.io/anthropic" || calls[0].opts.APIMode != "anthropic_messages" {
			t.Fatalf("resolved calls %+v", calls)
		}
		wire := client.sent[len(client.sent)-1]
		wireTools := auxDictList(wire["tools"])
		if _, has := wireTools[len(wireTools)-1]["cache_control"]; has {
			t.Fatal("a third party gateway gets no tool marker")
		}
		if _, has := tools[len(tools)-1]["cache_control"]; has {
			t.Fatal("caller tools must stay unmarked")
		}
		inner := 0
		for _, m := range auxDictList(wire["messages"]) {
			if parts, ok := m["content"].([]any); ok {
				inner += len(collectCacheControls(parts))
			} else if parts, ok := m["content"].([]map[string]any); ok {
				inner += len(collectCacheControls(parts))
			}
		}
		if inner == 0 {
			t.Fatalf("message content parts keep their markers %v", wire["messages"])
		}
	})
}
