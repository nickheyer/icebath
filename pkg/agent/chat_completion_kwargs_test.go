package agent

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
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
	before := timeutil.Monotonic()
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
	a.unavailableFallbackKeys = map[[3]string]bool{{"deepseek", "m", ""}: true}
	a.fallbackChain = []map[string]any{{"provider": "openai-codex", "model": "gpt-5"}, {"provider": "deepseek", "model": "m"}}
	if a.tryActivateFallback(context.Background(), provider.ReasonServerError) || a.fallbackIndex != 2 {
		t.Fatalf("activated or wrong index %d", a.fallbackIndex)
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
	if fn, _ := jsonutil.AsMap(out[0]["function"]); fn["name"] != "icebath_tool_search" {
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
	tc := row["tool_calls"].([]any)[0].(*jsonutil.Object)
	if tc.ValueOr("id", nil) != "call_abc" || tc.ValueOr("call_id", nil) != "call_abc" || tc.ValueOr("response_item_id", nil) != "fc_123" {
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

// Agent on a primary route with one resolvable fallback entry, its config written to the fixture home
func cooldownFallbackAgent(t *testing.T, configYAML string) *AIAgent {
	t.Helper()
	a := newRestoreAgent(t, configYAML, nil, "custom", "https://ollama.com/v1", nil)
	a.provider, a.requestedProvider, a.model, a.apiMode = "ollama-cloud", "ollama-cloud", "glm-5.2", "chat_completions"
	a.fallbackChain, a.fallbackIndex, a.fallbackActivated = []map[string]any{{"provider": "openai-codex", "model": "gpt-5.5"}}, 0, false
	a.credentialPool = isolationPool(a, "ollama-cloud")
	a.transportCache = map[string]provider.Transport{}
	recordIsolationResolves(t, isolationClient{apiKey: "codex-key", baseURL: "https://chatgpt.com/backend-api/codex"}, "gpt-5.5")
	recordIsolationPoolLoads(t, map[string]*provider.Pool{"openai-codex": isolationPool(a, "openai-codex")})
	return a
}

func TestFallbackSwitchBenchesPrimaryUntilValidFutureProviderReset(t *testing.T) {
	wall := func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	for _, c := range []struct {
		name    string
		ahead   float64
		whole   bool
		literal any
	}{
		{name: "fractional seconds round up", ahead: 90.2},
		{name: "whole seconds days ahead", ahead: 274_291, whole: true},
		{name: "no reset"},
		{name: "not a timestamp", literal: "not-a-timestamp"},
		{name: "reset in the past", ahead: -1, whole: true},
	} {
		a := cooldownFallbackAgent(t, "")
		a.rateLimitedUntil = 0
		wallBefore := wall()
		resetAt, reset := c.literal, 0.0
		if c.ahead != 0 {
			reset = wallBefore + c.ahead
			resetAt = reset
			if c.whole {
				reset = math.Floor(reset)
				resetAt = int(reset)
			}
		}
		monoBefore := timeutil.Monotonic()
		activated := a.tryActivateFallbackAt(a.homeContext(), provider.ReasonRateLimit, resetAt)
		monoAfter, wallAfter := timeutil.Monotonic(), wall()
		if !activated {
			t.Fatalf("%s: fallback not activated", c.name)
		}
		if a.model != "gpt-5.5" {
			t.Fatalf("%s: model %q", c.name, a.model)
		}
		low, high := 60.0, 60.0
		if c.ahead > 0 {
			low, high = math.Ceil(reset-wallAfter), math.Ceil(reset-wallBefore)
		}
		if a.rateLimitedUntil < monoBefore+low || a.rateLimitedUntil > monoAfter+high {
			t.Fatalf("%s: cooldown %.3f s want %v..%v s", c.name, a.rateLimitedUntil-monoBefore, low, high)
		}
	}
}

func TestMinSwitchResetSecondsKeepsPrimaryWhenResetIsImminent(t *testing.T) {
	for _, c := range []struct {
		threshold int
		switched  bool
	}{{0, true}, {120, false}} {
		a := cooldownFallbackAgent(t, fmt.Sprintf("fallback:\n  min_switch_reset_seconds: %d\n", c.threshold))
		a.rateLimitedUntil = 0
		resetAt := float64(time.Now().UnixNano())/1e9 + 30
		activated := a.tryActivateFallbackAt(a.homeContext(), provider.ReasonRateLimit, resetAt)
		if activated != c.switched {
			t.Fatalf("threshold %d: activated %v want %v", c.threshold, activated, c.switched)
		}
		if (a.model == "gpt-5.5") != c.switched {
			t.Fatalf("threshold %d: model %q", c.threshold, a.model)
		}
		if (a.rateLimitedUntil != 0) != c.switched {
			t.Fatalf("threshold %d: cooldown armed %v", c.threshold, a.rateLimitedUntil != 0)
		}
	}
}

func TestRateLimitExhaustionKeeps60sCooldown(t *testing.T) {
	a := cooldownFallbackAgent(t, "")
	a.rateLimitedUntil = 0
	before := timeutil.Monotonic()
	if !a.tryActivateFallback(a.homeContext(), provider.ReasonRateLimit) {
		t.Fatal("first activation")
	}
	after := timeutil.Monotonic()
	armed := a.rateLimitedUntil
	if armed < before+60 || armed > after+60 {
		t.Fatalf("cooldown %.3f s want 60 s", armed-before)
	}
	if a.tryActivateFallback(a.homeContext(), provider.ReasonRateLimit) {
		t.Fatal("chain should be exhausted")
	}
	if a.rateLimitedUntil != armed {
		t.Fatalf("exhaustion moved the 60 s cooldown by %.3f s", a.rateLimitedUntil-armed)
	}
}

func TestCooldownNeverShrinksExistingWindow(t *testing.T) {
	a := cooldownFallbackAgent(t, "")
	far := timeutil.Monotonic() + 999
	a.rateLimitedUntil = far
	if !a.tryActivateFallback(a.homeContext(), "") || a.tryActivateFallback(a.homeContext(), "") {
		t.Fatal("activation sequence")
	}
	if a.rateLimitedUntil != far {
		t.Fatalf("cooldown shrank by %.3f s", far-a.rateLimitedUntil)
	}
}

func TestBackoffCapsAtFourHours(t *testing.T) {
	a := cooldownFallbackAgent(t, "")
	a.rateLimitedUntil, a.rateLimitBackoffCount = 0, 10
	before := timeutil.Monotonic()
	a.tryActivateFallback(a.homeContext(), provider.ReasonRateLimit)
	after := timeutil.Monotonic()
	if a.rateLimitedUntil < before+14400 || a.rateLimitedUntil > after+14400 {
		t.Fatalf("cooldown %.3f s want 14400 s", a.rateLimitedUntil-before)
	}
}

func TestGeminiFastFallbackPoolRecovery(t *testing.T) {
	a := isolationAgent(t, "ollama-cloud", "glm-5.2", "https://ollama.com/v1", "chat_completions", nil)
	now := float64(time.Now().UnixNano()) / 1e9
	live := func(i int) provider.PooledCredential {
		return provider.PooledCredential{Provider: "gemini", ID: fmt.Sprintf("gemini-entry-%d", i), AccessToken: fmt.Sprintf("key-gemini-%d", i),
			BaseURL: "https://gemini.example.com/v1"}
	}
	benched := func(i int) provider.PooledCredential {
		e := live(i)
		e.LastStatus, e.LastStatusAt, e.LastErrorResetAt = provider.StatusExhausted, now, now+3600
		return e
	}
	if !poolMayRecoverFromRateLimit(isolationPool(a, "gemini", live(0), live(1), live(2))) {
		t.Fatal("multi entry pool should recover")
	}
	if poolMayRecoverFromRateLimit(isolationPool(a, "gemini", benched(0), benched(1), benched(2))) {
		t.Fatal("exhausted pool should skip rotation")
	}
	if poolMayRecoverFromRateLimit(isolationPool(a, "gemini", live(0))) || poolMayRecoverFromRateLimit(nil) {
		t.Fatal("single entry or missing pool should fall back")
	}
}
