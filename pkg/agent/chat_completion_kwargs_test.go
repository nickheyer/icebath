package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Bare agent on a route with its derived host fields under a temp home
func kwargsTestAgent(t *testing.T, providerName, baseURL, model, apiMode string) *AIAgent {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{provider: providerName, baseURL: baseURL, model: model, apiMode: apiMode, sessionID: "sess-1"}
	a.setBaseURL(baseURL)
	return a
}

func TestRewritePromptModelIdentityOnlyLastOccurrenceIsRewritten(t *testing.T) {
	a := &AIAgent{cachedSystemPrompt: "Model: decoy-from-memory\nbody\nModel: primary\nProvider: openrouter\n"}
	a.rewritePromptModelIdentity("gemma4:e2b-mlx", "custom")
	if !strings.Contains(a.cachedSystemPrompt, "Model: decoy-from-memory") {
		t.Fatalf("earlier occurrence rewritten: %q", a.cachedSystemPrompt)
	}
	if !strings.Contains(a.cachedSystemPrompt, "Model: gemma4:e2b-mlx\nProvider: custom\n") {
		t.Fatalf("last occurrence not rewritten: %q", a.cachedSystemPrompt)
	}
}

func TestRewritePromptModelIdentityNoopWhenPromptMissingOrEmpty(t *testing.T) {
	a := &AIAgent{}
	a.rewritePromptModelIdentity("m", "p")
	if a.cachedSystemPrompt != "" {
		t.Fatalf("empty prompt changed: %q", a.cachedSystemPrompt)
	}
}

func TestFallbackEntryKeyNormalizesEveryAxis(t *testing.T) {
	got := fallbackEntryKey(map[string]any{"provider": " OpenRouter ", "model": " m ", "base_url": " https://x/v1/ "})
	if got != [3]string{"openrouter", "m", "https://x/v1"} {
		t.Fatalf("key %v", got)
	}
	if fallbackEntryKey(map[string]any{"provider": nil}) != [3]string{"", "", ""} {
		t.Fatal("None axes must key as empty strings")
	}
}

func TestTryActivateFallbackExhaustedChainArmsShortCooldown(t *testing.T) {
	a := kwargsTestAgent(t, "openrouter", "https://openrouter.ai/api/v1", "a/b", "chat_completions")
	if a.tryActivateFallback(context.Background(), provider.ReasonServerError) {
		t.Fatal("empty chain activated")
	}
	if a.rateLimitedUntil != 0 {
		t.Fatal("empty chain armed a cooldown")
	}
	a.fallbackChain = []map[string]any{{"provider": "openrouter"}, {"model": "x"}}
	before := pythread.Monotonic()
	if a.tryActivateFallback(context.Background(), provider.ReasonServerError) {
		t.Fatal("malformed entries activated")
	}
	if a.fallbackIndex != 2 || a.rateLimitedUntil < before+fallbackExhaustedCooldownSeconds-0.5 {
		t.Fatalf("index %d until %v", a.fallbackIndex, a.rateLimitedUntil)
	}
}

func TestTryActivateFallbackSkipsTheBackendThatJustFailed(t *testing.T) {
	a := kwargsTestAgent(t, "openrouter", "https://openrouter.ai/api/v1", "a/b", "chat_completions")
	a.fallbackChain = []map[string]any{{"provider": "openrouter", "model": "a/b", "base_url": "https://openrouter.ai/api/v1"}}
	if a.tryActivateFallback(context.Background(), provider.ReasonServerError) || a.fallbackActivated {
		t.Fatal("same backend activated")
	}
}

func TestTryActivateFallbackSkipsUnentitledAndUnavailableEntries(t *testing.T) {
	a := kwargsTestAgent(t, "openrouter", "https://openrouter.ai/api/v1", "a/b", "chat_completions")
	a.entitlementRejectedModels = map[[2]string]bool{{"openai-codex", "gpt-5"}: true}
	a.unavailableFallbackKeys = map[[3]string]bool{{"nous", "m", ""}: true}
	a.fallbackChain = []map[string]any{{"provider": "openai-codex", "model": "gpt-5"}, {"provider": "nous", "model": "m"}}
	if a.tryActivateFallback(context.Background(), provider.ReasonServerError) || a.fallbackIndex != 2 {
		t.Fatalf("activated or wrong index %d", a.fallbackIndex)
	}
}

func TestFallbackEntryUnavailableWithoutNetworkNeedsANousToken(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	if r := fallbackEntryUnavailableWithoutNetwork(ctx, map[string]any{"provider": "openrouter"}); r != "" {
		t.Fatalf("non nous entry skipped: %q", r)
	}
	if r := fallbackEntryUnavailableWithoutNetwork(ctx, map[string]any{"provider": " Nous "}); r != "nous_token_missing" {
		t.Fatalf("reason %q", r)
	}
}

func TestArmRateLimitCooldownDoublesFromSixtySeconds(t *testing.T) {
	a := &AIAgent{provider: "openrouter"}
	if got := a.armRateLimitCooldown(provider.ReasonServerError, nil); got != nil {
		t.Fatalf("non rate limit armed %v", got)
	}
	first, second := a.armRateLimitCooldown(provider.ReasonRateLimit, nil), a.armRateLimitCooldown(provider.ReasonRateLimit, nil)
	if first != int64(60) || second != int64(120) {
		t.Fatalf("backoff %v %v", first, second)
	}
	a.fallbackActivated, a.primaryRuntime = true, map[string]any{"provider": "anthropic"}
	if a.armRateLimitCooldown(provider.ReasonRateLimit, nil) != nil {
		t.Fatal("chain switch from an active fallback armed the primary cooldown")
	}
}

func TestBufferFallbackNoticeKeepsEveryNotice(t *testing.T) {
	a := &AIAgent{}
	a.bufferFallbackNotice("one")
	a.bufferFallbackNotice("two")
	if len(a.pendingFallbackNotice) != 2 || a.pendingFallbackNotice[1] != "two" {
		t.Fatalf("pending %v", a.pendingFallbackNotice)
	}
}

func TestConsumeEphemeralMaxOutputIsOneShot(t *testing.T) {
	a := &AIAgent{ephemeralMaxOutputTokens: 4096}
	if a.consumeEphemeralMaxOutput() != 4096 || a.consumeEphemeralMaxOutput() != nil {
		t.Fatal("ephemeral cap not consumed exactly once")
	}
}

func TestAliasToolSearchBridgeForXAIRoundTripsThroughTheTransport(t *testing.T) {
	a := kwargsTestAgent(t, "xai", "https://api.x.ai/v1", "grok-4", "chat_completions")
	tr := a.getTransport("")
	toolsIn := []map[string]any{{"type": "function", "function": map[string]any{"name": "tool_search", "parameters": map[string]any{}}}}
	out := a.aliasToolSearchBridgeForXAI(tr, toolsIn)
	if out[0]["function"].(map[string]any)["name"] != "icebath_tool_search" {
		t.Fatalf("bridge not aliased: %v", out)
	}
	if toolsIn[0]["function"].(map[string]any)["name"] != "tool_search" {
		t.Fatal("input tools mutated")
	}
	resp, err := tr.NormalizeResponse(map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{
		"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
			"function": map[string]any{"name": "icebath_tool_search", "arguments": "{}"}}}}}}}, map[string]any{"model": "grok-4"})
	if err != nil || len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Name != "tool_search" {
		t.Fatalf("alias not reversed: %v %v", resp, err)
	}
	other := kwargsTestAgent(t, "openrouter", "https://openrouter.ai/api/v1", "x", "chat_completions")
	if got := other.aliasToolSearchBridgeForXAI(tr, toolsIn); got[0]["function"].(map[string]any)["name"] != "tool_search" {
		t.Fatal("non xAI route aliased")
	}
}

func TestProviderPreferencesForAgentDropsFalsyAndInvalidSort(t *testing.T) {
	a := kwargsTestAgent(t, "openrouter", "https://openrouter.ai/api/v1", "a/b", "chat_completions")
	a.providersAllowed, a.providerSort, a.providerRequireParameters = []any{"x"}, "bogus", true
	got := a.providerPreferencesForAgent(context.Background())
	if len(got) != 2 || got["require_parameters"] != true || got["only"] == nil {
		t.Fatalf("prefs %v", got)
	}
}

func TestBuildAssistantMessageExtractsInlineThinkAndPadsToolCallReasoning(t *testing.T) {
	a := kwargsTestAgent(t, "deepseek", "https://api.deepseek.com/v1", "deepseek-v4", "chat_completions")
	msg := &state.Message{Role: "assistant", Content: "<think>plan it</think>answer",
		ToolCalls: []state.ToolCall{{ID: "call_abc|fc_123", Type: "function", Function: state.ToolCallFunction{Name: "f", Arguments: "{}"}}}}
	row := a.buildAssistantMessage(&provider.Response{Message: msg}, "tool_calls")
	if row["content"] != "answer" || row["reasoning"] != "plan it" || row["reasoning_content"] != "plan it" || row["finish_reason"] != "tool_calls" {
		t.Fatalf("row %v", row)
	}
	tc := row["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != "call_abc" || tc["call_id"] != "call_abc" || tc["response_item_id"] != "fc_123" {
		t.Fatalf("tool call %v", tc)
	}
}

func TestBuildAPIKwargsChatCompletionsCarriesModelMessagesAndTools(t *testing.T) {
	a := kwargsTestAgent(t, "openrouter", "https://openrouter.ai/api/v1", "a/b", "chat_completions")
	a.tools = []any{map[string]any{"type": "function", "function": map[string]any{"name": "f"}}}
	kwargs, err := a.buildAPIKwargs([]map[string]any{{"role": "user", "content": "hi"}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if kwargs["model"] != "a/b" || len(kwargs["messages"].([]map[string]any)) != 1 || len(kwargs["tools"].([]map[string]any)) != 1 {
		t.Fatalf("kwargs %v", kwargs)
	}
}
