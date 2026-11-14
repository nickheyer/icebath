package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Fixture agent on a primary endpoint with an optional fallback chain whose client builds are doubled
func newRestoreAgent(t *testing.T, configYAML string, fallback []map[string]any, providerName, baseURL string, overrides map[string]any) *AIAgent {
	t.Helper()
	patchContextLength(t, 200_000)
	savedCtor, savedSwap, savedLoad := openAIClientConstructor, swapPoolCredential, agentLoadPool
	t.Cleanup(func() { openAIClientConstructor, swapPoolCredential, agentLoadPool = savedCtor, savedSwap, savedLoad })
	openAIClientConstructor = func(map[string]any) (any, error) { return &struct{ name string }{"client"}, nil }
	return newFixtureAgentWithConfig(t, configYAML, AIAgentOptions{Provider: providerName, BaseURL: baseURL,
		APIKey: "test-key-12345678", RequestOverrides: overrides, FallbackModel: fallback})
}

// Records every credential swap the restore asks for
func recordRestoreSwaps() *[]provider.PooledCredential {
	swaps := &[]provider.PooledCredential{}
	swapPoolCredential = func(_ context.Context, _ *AIAgent, e provider.PooledCredential) (bool, error) {
		*swaps = append(*swaps, e)
		return true, nil
	}
	return swaps
}

// Records each pool key the primary pool load reads and answers with pool
func recordRestorePoolLoads(pool *provider.Pool) *[]string {
	keys := &[]string{}
	agentLoadPool = func(_ provider.PoolEnv, key string) *provider.Pool {
		*keys = append(*keys, key)
		return pool
	}
	return keys
}

// Captures every status message the agent emits
func captureStatuses(a *AIAgent) *[]string {
	emitted := &[]string{}
	a.statusCallback = func(_ string, msg any) { *emitted = append(*emitted, fmt.Sprint(msg)) }
	return emitted
}

func restorePool(a *AIAgent, providerName string, entries ...provider.PooledCredential) *provider.Pool {
	return provider.NewPool(agentPoolEnv(a.homeContext()), providerName, entries)
}

func mustActivate(t *testing.T, a *AIAgent, reason provider.FailoverReason) {
	t.Helper()
	if !a.tryActivateFallback(context.Background(), reason) {
		t.Fatal("fallback did not activate")
	}
}

func mustRestore(t *testing.T, a *AIAgent) bool {
	t.Helper()
	restored, err := a.restorePrimaryRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return restored
}

var openrouterFallback = []map[string]any{{"provider": "openrouter", "model": "anthropic/claude-sonnet-4", "api_key": "fallback-key-1234"}}

const restoreBaseURL = "https://my-llm.example.com/v1"

func TestRestorePrimaryRuntimeNoopWhenNotFallback(t *testing.T) {
	a := newRestoreAgent(t, "", nil, "custom", restoreBaseURL, nil)
	if a.fallbackActivated {
		t.Fatal("fresh agent on fallback")
	}
	if mustRestore(t, a) {
		t.Fatal("restored without a fallback")
	}
}

func TestRestorePrimaryRuntimeReasoningReplayVerdictDoesNotFollowTheSessionToAnotherRoute(t *testing.T) {
	a := newRestoreAgent(t, "", openrouterFallback, "custom", restoreBaseURL, nil)
	verdict := func() [2]bool { return [2]bool{a.codexReasoningReplayEnabled, a.codexReasoningReplayRejected} }
	a.codexReasoningReplayEnabled, a.codexReasoningReplayRejected = false, true
	mustActivate(t, a, "")
	if verdict() != [2]bool{true, false} {
		t.Fatalf("fallback verdict %v", verdict())
	}
	a.codexReasoningReplayEnabled, a.codexReasoningReplayRejected = false, true
	if !mustRestore(t, a) {
		t.Fatal("restore declined")
	}
	if verdict() != [2]bool{true, false} {
		t.Fatalf("restore verdict %v", verdict())
	}
}

func TestRestorePrimaryRuntimeDoesNotLabelTemporaryModelRestoreAsFallbackRecovery(t *testing.T) {
	a := newRestoreAgent(t, "", nil, "custom", restoreBaseURL, nil)
	a.model, a.provider, a.fallbackActivated = "temporary-model", "openrouter", true
	emitted := captureStatuses(a)
	if !mustRestore(t, a) {
		t.Fatal("restore declined")
	}
	for _, m := range *emitted {
		if low := strings.ToLower(m); strings.Contains(low, "restored") || strings.Contains(low, "fallback") {
			t.Fatalf("restore labeled %q", m)
		}
	}
}

// Context engine whose next model update raises once
type failingUpdateEngine struct {
	ContextEngine
	failures int
}

func (e *failingUpdateEngine) UpdateModel(model string, contextLength int, baseURL, apiKey, providerName, apiMode string) {
	if e.failures > 0 {
		e.failures--
		panic(errors.New("transient restore failure"))
	}
	e.ContextEngine.UpdateModel(model, contextLength, baseURL, apiKey, providerName, apiMode)
}

func TestRestorePrimaryRuntimeRestoreRetryPreservesFallbackIdentityAfterPartialFailure(t *testing.T) {
	a := newRestoreAgent(t, "", openrouterFallback, "custom", restoreBaseURL, nil)
	a.model = "primary-model"
	a.primaryRuntime["model"] = "primary-model"
	mustActivate(t, a, "")
	emitted := captureStatuses(a)
	a.contextCompressor = &failingUpdateEngine{ContextEngine: a.contextCompressor, failures: 1}
	if mustRestore(t, a) {
		t.Fatal("first restore succeeded through the failure")
	}
	if !mustRestore(t, a) {
		t.Fatal("retry declined")
	}
	if len(*emitted) != 1 || !strings.Contains((*emitted)[0], "primary-model") || !strings.Contains((*emitted)[0], "anthropic/claude-sonnet-4") {
		t.Fatalf("notices %q", *emitted)
	}
}

func TestRestorePrimaryRuntimeResetsFallbackIndex(t *testing.T) {
	a := newRestoreAgent(t, "", []map[string]any{{"provider": "openrouter", "model": "model-a", "api_key": "fallback-key-1234"}, {"provider": "anthropic", "model": "model-b", "api_key": "fallback-key-1234"}}, "custom", restoreBaseURL, nil)
	a.tryActivateFallback(context.Background(), "")
	if a.fallbackIndex != 1 {
		t.Fatalf("index %d", a.fallbackIndex)
	}
	mustRestore(t, a)
	if a.fallbackIndex != 0 {
		t.Fatalf("index %d after restore", a.fallbackIndex)
	}
}

func TestRestorePrimaryRuntimeRestoresCompressorState(t *testing.T) {
	a := newRestoreAgent(t, "", openrouterFallback, "custom", restoreBaseURL, nil)
	ctxLen, threshold := engineContextLength(a.contextCompressor), engineThresholdTokens(a.contextCompressor)
	a.tryActivateFallback(context.Background(), "")
	cc := a.contextCompressor.(*ContextCompressor)
	cc.SetContextLength(32000)
	cc.SetThresholdTokens(25600)
	mustRestore(t, a)
	if gotLen, gotThreshold := engineContextLength(a.contextCompressor), engineThresholdTokens(a.contextCompressor); gotLen != ctxLen || gotThreshold != threshold {
		t.Fatalf("compressor %d/%d want %d/%d", gotLen, gotThreshold, ctxLen, threshold)
	}
}

func TestRestorePrimaryRuntimeRestoresPromptCachingFlag(t *testing.T) {
	a := newRestoreAgent(t, "", nil, "custom", restoreBaseURL, nil)
	original := a.usePromptCaching
	a.fallbackActivated, a.usePromptCaching = true, !original
	mustRestore(t, a)
	if a.usePromptCaching != original {
		t.Fatalf("caching %v", a.usePromptCaching)
	}
}

func TestRestorePrimaryRuntimeRestoresRequestOverrides(t *testing.T) {
	original := map[string]any{"extra_body": map[string]any{"reasoning": map[string]any{"effort": "medium"}}}
	a := newRestoreAgent(t, "", nil, "custom", restoreBaseURL, original)
	a.fallbackActivated = true
	a.requestOverrides = map[string]any{"extra_body": map[string]any{"fallback_only": true}}
	if !mustRestore(t, a) {
		t.Fatal("restore declined")
	}
	if !reflect.DeepEqual(a.requestOverrides, original) {
		t.Fatalf("overrides %v", a.requestOverrides)
	}
	snapshot, _ := a.primaryRuntime["request_overrides"].(map[string]any)
	a.requestOverrides["probe"] = true
	if _, shared := snapshot["probe"]; shared {
		t.Fatal("overrides alias the snapshot")
	}
}

func TestRestorePrimaryRuntimeRestoreSkipsCrossProviderPoolEntry(t *testing.T) {
	a := newRestoreAgent(t, "", openrouterFallback, "custom", "https://primary.example.com/v1", nil)
	originalBase := a.baseURL
	a.tryActivateFallback(context.Background(), "")
	a.credentialPool = restorePool(a, "openrouter", provider.PooledCredential{Provider: "openrouter", ID: "fallback-entry", Label: "fallback",
		AccessToken: "fallback-key", BaseURL: "https://openrouter.ai/api/v1"})
	swaps := recordRestoreSwaps()
	if !mustRestore(t, a) {
		t.Fatal("restore declined")
	}
	if a.provider != "custom" || a.baseURL != originalBase {
		t.Fatalf("route %s %s", a.provider, a.baseURL)
	}
	if len(*swaps) != 0 {
		t.Fatalf("swapped %v", *swaps)
	}
}

func TestRestorePrimaryRuntimeRestoreKeepsPrimaryBaseURLWhenFallbackPoolAttached(t *testing.T) {
	a := newRestoreAgent(t, "", []map[string]any{{"provider": "deepseek", "model": "deepseek-v4-flash", "api_key": "fallback-key-1234"}}, "openai-api", "https://api.openai.com/v1", nil)
	primaryBase, primaryProvider := a.baseURL, a.provider
	a.tryActivateFallback(context.Background(), "")
	a.credentialPool = restorePool(a, "deepseek", provider.PooledCredential{Provider: "deepseek", ID: "dsk-1", Label: "deepseek-key",
		AccessToken: "sk-deepseek-xxx", BaseURL: "https://api.deepseek.com/v1"})
	swaps := recordRestoreSwaps()
	primaryPool := restorePool(a, primaryProvider)
	keys := recordRestorePoolLoads(primaryPool)
	if !mustRestore(t, a) {
		t.Fatal("restore declined")
	}
	if a.provider != primaryProvider || a.baseURL != primaryBase || strings.Contains(a.baseURL, "deepseek") {
		t.Fatalf("route %s %s", a.provider, a.baseURL)
	}
	if a.credentialPool != primaryPool {
		t.Fatal("primary pool not attached")
	}
	if !reflect.DeepEqual(*keys, []string{primaryProvider}) {
		t.Fatalf("loads %v", *keys)
	}
	if len(*swaps) != 0 {
		t.Fatalf("swapped %v", *swaps)
	}
}

func TestRestorePrimaryRuntimeRestoreClearsFallbackPoolWhenPrimaryPoolReloadFails(t *testing.T) {
	a := newRestoreAgent(t, "", nil, "openai-api", "https://api.openai.com/v1", nil)
	a.fallbackActivated = true
	a.credentialPool = restorePool(a, "deepseek")
	agentLoadPool = func(provider.PoolEnv, string) *provider.Pool { panic(errors.New("auth store unavailable")) }
	if !mustRestore(t, a) {
		t.Fatal("restore declined")
	}
	if a.provider != "openai-api" || a.credentialPool != nil {
		t.Fatalf("provider %s pool %v", a.provider, a.credentialPool)
	}
}

func TestRestorePrimaryRuntimeRestoreSwapsMatchingCustomPoolEntry(t *testing.T) {
	a := newRestoreAgent(t, "custom_providers:\n  - name: myllm\n    base_url: https://my-llm.example.com/v1\n", nil, "custom", restoreBaseURL, nil)
	a.fallbackActivated = true
	a.credentialPool = restorePool(a, "custom:myllm", provider.PooledCredential{Provider: "custom:myllm", ID: "custom-entry", Label: "myllm",
		AccessToken: "custom-key", BaseURL: restoreBaseURL})
	swaps := recordRestoreSwaps()
	if !mustRestore(t, a) {
		t.Fatal("restore declined")
	}
	if len(*swaps) != 1 || (*swaps)[0].ID != "custom-entry" {
		t.Fatalf("swaps %v", *swaps)
	}
}

func TestRestorePrimaryRuntimeRestoreReloadsNamedCustomPoolByScopedKey(t *testing.T) {
	cfg := "custom_providers:\n  - name: Legacy Provider\n    base_url: https://legacy.example/v1\n" +
		"providers:\n  gemini-no-filter:\n    name: Gemini Display\n    api: https://generativelanguage.googleapis.com/v1beta\n"
	a := newRestoreAgent(t, cfg, nil, "custom:gemini-no-filter", "https://generativelanguage.googleapis.com/v1beta", nil)
	a.fallbackActivated = true
	a.credentialPool = restorePool(a, "openrouter")
	swaps := recordRestoreSwaps()
	primaryPool := restorePool(a, "custom:gemini-display", provider.PooledCredential{Provider: "custom:gemini-display", ID: "gemini-key", Label: "gemini", AccessToken: "gemini-key"})
	keys := recordRestorePoolLoads(primaryPool)
	if !mustRestore(t, a) {
		t.Fatal("restore declined")
	}
	if a.credentialPool != primaryPool {
		t.Fatal("primary pool not attached")
	}
	if !reflect.DeepEqual(*keys, []string{"gemini-no-filter"}) {
		t.Fatalf("loads %v", *keys)
	}
	if len(*swaps) != 1 || (*swaps)[0].ID != "gemini-key" {
		t.Fatalf("swaps %v", *swaps)
	}
}

func TestRestorePrimaryRuntimeRestoreNamedCustomPoolWrongEndpointFailsClosed(t *testing.T) {
	a := newRestoreAgent(t, "", nil, "gemini-no-filter", "https://fallback.example/v1", nil)
	cfg := "providers:\n  gemini-no-filter:\n    name: Gemini No Filter\n    provider_key: gemini-no-filter\n    base_url: https://generativelanguage.googleapis.com/v1beta\n"
	if err := os.WriteFile(filepath.Join(a.env.Home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a.fallbackActivated = true
	a.credentialPool = restorePool(a, "custom:gemini-no-filter")
	swaps := recordRestoreSwaps()
	keys := recordRestorePoolLoads(nil)
	if !mustRestore(t, a) {
		t.Fatal("restore declined")
	}
	if a.credentialPool != nil {
		t.Fatalf("pool %v", a.credentialPool)
	}
	if !reflect.DeepEqual(*keys, []string{"gemini-no-filter"}) {
		t.Fatalf("loads %v", *keys)
	}
	if len(*swaps) != 0 {
		t.Fatalf("swapped %v", *swaps)
	}
}

func TestRestoreInRunConversationFullCycleFallbackThenRestore(t *testing.T) {
	a := newRestoreAgent(t, "", openrouterFallback, "custom", restoreBaseURL, nil)
	mustActivate(t, a, "")
	if !a.fallbackActivated || a.model != "anthropic/claude-sonnet-4" || a.provider != "openrouter" || a.fallbackIndex != 1 {
		t.Fatalf("fallback %v %s %s %d", a.fallbackActivated, a.model, a.provider, a.fallbackIndex)
	}
	if !mustRestore(t, a) {
		t.Fatal("restore declined")
	}
	if a.fallbackActivated || a.fallbackIndex != 0 || a.provider != "custom" || a.baseURL != restoreBaseURL {
		t.Fatalf("restored %v %d %s %s", a.fallbackActivated, a.fallbackIndex, a.provider, a.baseURL)
	}
}

func TestRateLimitCooldownRestoreBlockedDuringCooldown(t *testing.T) {
	a := newRestoreAgent(t, "", openrouterFallback, "custom", restoreBaseURL, nil)
	a.tryActivateFallback(context.Background(), "")
	if !a.fallbackActivated {
		t.Fatal("fallback did not activate")
	}
	a.rateLimitedUntil = pythread.Monotonic() + 60
	if mustRestore(t, a) {
		t.Fatal("restored during cooldown")
	}
	if !a.fallbackActivated {
		t.Fatal("left the fallback during cooldown")
	}
}

func TestRateLimitCooldownCooldownSetOnRateLimitReason(t *testing.T) {
	a := newRestoreAgent(t, "", openrouterFallback, "custom", restoreBaseURL, nil)
	before := pythread.Monotonic()
	a.tryActivateFallback(context.Background(), provider.ReasonRateLimit)
	if a.rateLimitedUntil <= before+50 {
		t.Fatalf("cooldown until %v from %v", a.rateLimitedUntil, before)
	}
}

func TestRateLimitCooldownCooldownNotSetWhenAlreadyOnFallback(t *testing.T) {
	a := newRestoreAgent(t, "", []map[string]any{{"provider": "openrouter", "model": "model-a", "api_key": "fallback-key-1234"}, {"provider": "anthropic", "model": "model-b", "api_key": "fallback-key-1234"}}, "custom", restoreBaseURL, nil)
	a.tryActivateFallback(context.Background(), provider.ReasonRateLimit)
	first := a.rateLimitedUntil
	a.tryActivateFallback(context.Background(), provider.ReasonRateLimit)
	if a.rateLimitedUntil != first {
		t.Fatalf("cooldown advanced from %v to %v", first, a.rateLimitedUntil)
	}
}
