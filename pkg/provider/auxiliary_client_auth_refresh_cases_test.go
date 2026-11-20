package provider

import (
	"context"
	"reflect"
	"testing"
)

func TestAnthropicCompatImageConversionUrlBasedDetection(t *testing.T) {
	if !auxIsAnthropicCompatEndpoint("custom", "https://api.minimax.io/anthropic") || !auxIsAnthropicCompatEndpoint("custom", "https://example.com/anthropic/v1") {
		t.Fatal("anthropic path not detected")
	}
	if auxIsAnthropicCompatEndpoint("custom", "https://api.openai.com/v1") {
		t.Fatal("openai endpoint detected as anthropic")
	}
}

// First content block of the first converted message
func auxFirstConvertedBlock(t *testing.T, messages []any) map[string]any {
	t.Helper()
	result := auxConvertOpenAIImagesToAnthropic(messages)
	first, _ := result.([]any)
	if len(first) == 0 {
		t.Fatalf("converted %v", result)
	}
	content, _ := first[0].(map[string]any)["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("converted %v", result)
	}
	block, _ := content[0].(map[string]any)
	return block
}

func TestAnthropicCompatImageConversionUrlImageConverted(t *testing.T) {
	block := auxFirstConvertedBlock(t, []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/img.jpg"}}}}})
	source, _ := block["source"].(map[string]any)
	if block["type"] != "image" || source["type"] != "url" || source["url"] != "https://example.com/img.jpg" {
		t.Fatalf("block %v", block)
	}
}

func TestAnthropicCompatImageConversionUrlVideoConvertedToVideoBlock(t *testing.T) {
	block := auxFirstConvertedBlock(t, []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "video_url", "video_url": map[string]any{"url": "https://example.com/clip.mp4"}}}}})
	if block["type"] != "video" || !reflect.DeepEqual(block["source"], map[string]any{"type": "url", "url": "https://example.com/clip.mp4"}) {
		t.Fatalf("block %v", block)
	}
}

// Fake client failing with a 401 on its first call and answering sync-ok afterwards
func auxFailingThenSuccessClient(base string) *auxCallFakeClient {
	return auxScriptedCallClient(base, "k", auxAuth401("Provided authentication token is expired"), auxCallOK("sync-ok"))
}

func TestAuxiliaryAuthRefreshRetryCallLlmRefreshesCodexOn401ForVision(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	failing := auxFailingThenSuccessClient("https://chatgpt.com/backend-api/codex")
	fresh := auxScriptedCallClient("https://chatgpt.com/backend-api/codex", "k", auxCallOK("fresh-sync"))
	resolves := 0
	prevVision := auxResolveVisionProviderClientFn
	auxResolveVisionProviderClientFn = func(context.Context, string, string, string, any, bool, map[string]any) (string, AuxClient, string, error) {
		resolves++
		if resolves == 1 {
			return "openai-codex", failing, "gpt-5.4", nil
		}
		return "openai-codex", fresh, "gpt-5.4", nil
	}
	t.Cleanup(func() { auxResolveVisionProviderClientFn = prevVision })
	var refreshed [][2]string
	prevRefresh := auxRefreshProviderCredentialsFn
	auxRefreshProviderCredentialsFn = func(_ context.Context, provider, failedAPIKey string) bool {
		refreshed = append(refreshed, [2]string{provider, failedAPIKey})
		return true
	}
	t.Cleanup(func() { auxRefreshProviderCredentialsFn = prevRefresh })
	resp, err := CallLLM(ctx, map[string]any{"task": "vision", "provider": "openai-codex", "model": "gpt-5.4", "messages": auxUserMessages("hi")})
	if err != nil || auxActualContent(t, resp) != "fresh-sync" {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if len(refreshed) != 1 || refreshed[0] != [2]string{"openai-codex", ""} {
		t.Fatalf("refresh calls %v", refreshed)
	}
}

// Seeds one cached client for a provider under the bound home and returns its key
func auxSeedCachedClient(t *testing.T, ctx context.Context, provider string, client AuxClient, model string) auxClientCacheKey {
	t.Helper()
	key, _, err := auxClientCacheKeyFor(ctx, provider, false, "", nil, "", nil, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	auxClientCacheMu.Lock()
	auxClientCache[key] = auxCachedClient{Client: client, DefaultModel: model}
	auxClientCacheOrder = append(auxClientCacheOrder, key)
	auxClientCacheMu.Unlock()
	return key
}

func auxCacheHolds(key auxClientCacheKey) bool {
	auxClientCacheMu.Lock()
	defer auxClientCacheMu.Unlock()
	_, ok := auxClientCache[key]
	return ok
}

func TestAuxiliaryAuthRefreshRetryRefreshProviderCredentialsForceRefreshesAnthropicOauthAndEvictsCache(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxIsolateClientCache(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, name := range []string{"ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		t.Setenv(name, "")
	}
	stale := &auxCallFakeClient{}
	key := auxSeedCachedClient(t, ctx, "anthropic", stale, "claude-haiku-4-5-20251001")
	creds := map[string]any{"accessToken": "expired-token", "refreshToken": "refresh-token", "expiresAt": float64(0)}
	prevAuxRead, prevRead, prevRefresh, prevWrite := auxReadClaudeCodeCredentialsFn, readClaudeCodeCredentialsFn, refreshAnthropicOAuthPureFn, writeClaudeCodeCredentialsFn
	t.Cleanup(func() {
		auxReadClaudeCodeCredentialsFn, readClaudeCodeCredentialsFn, refreshAnthropicOAuthPureFn, writeClaudeCodeCredentialsFn = prevAuxRead, prevRead, prevRefresh, prevWrite
	})
	auxReadClaudeCodeCredentialsFn = func(context.Context) map[string]any { return creds }
	readClaudeCodeCredentialsFn = func(PoolEnv) map[string]any { return creds }
	var refreshCalls [][2]any
	refreshAnthropicOAuthPureFn = func(_ PoolEnv, refreshToken string, useJSON bool) (map[string]any, error) {
		refreshCalls = append(refreshCalls, [2]any{refreshToken, useJSON})
		return map[string]any{"access_token": "fresh-token", "refresh_token": "refresh-token-2", "expires_at_ms": float64(9999999999999)}, nil
	}
	var writes [][4]any
	writeClaudeCodeCredentialsFn = func(_ PoolEnv, accessToken, refreshToken string, expiresAtMS int64, _ []any, spent string) error {
		writes = append(writes, [4]any{accessToken, refreshToken, expiresAtMS, spent})
		return nil
	}
	if !auxRefreshProviderCredentials(ctx, "anthropic", "expired-token") {
		t.Fatal("refresh reported failure")
	}
	if auxCacheHolds(key) {
		t.Fatal("stale anthropic client still cached")
	}
	if len(refreshCalls) != 1 || refreshCalls[0] != [2]any{"refresh-token", false} {
		t.Fatalf("refresh calls %v", refreshCalls)
	}
	if len(writes) != 1 || writes[0] != [4]any{"fresh-token", "refresh-token-2", int64(9999999999999), "refresh-token"} {
		t.Fatalf("writes %v", writes)
	}
	if stale.closed.Load() {
		t.Fatal("evicted client was closed")
	}
}

func TestAuxiliaryAuthRefreshRetryRefreshProviderCredentialsRemintsVertexTokenAndEvictsCache(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxIsolateClientCache(t)
	stale := &auxCallFakeClient{}
	key := auxSeedCachedClient(t, ctx, "vertex", stale, "google/gemini-3-flash-preview")
	configCalls := 0
	prev := auxVertexConfigFn
	auxVertexConfigFn = func(context.Context) (string, string) {
		configCalls++
		return "ya29.FRESH", "https://aiplatform.googleapis.com/v1beta1/projects/p/locations/global/endpoints/openapi"
	}
	t.Cleanup(func() { auxVertexConfigFn = prev })
	if !auxRefreshProviderCredentials(ctx, "vertex", "") {
		t.Fatal("refresh reported failure")
	}
	if auxCacheHolds(key) || configCalls != 1 || stale.closed.Load() {
		t.Fatalf("cached %v config calls %d closed %v", auxCacheHolds(key), configCalls, stale.closed.Load())
	}
}

func TestAuxiliaryAuthRefreshRetryRefreshProviderCredentialsVertexReturnsFalseWhenUnminted(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	prev := auxVertexConfigFn
	auxVertexConfigFn = func(context.Context) (string, string) { return "", "" }
	t.Cleanup(func() { auxVertexConfigFn = prev })
	if auxRefreshProviderCredentials(ctx, "vertex", "") {
		t.Fatal("unminted vertex refreshed")
	}
}

func TestAuxiliaryAuthRefreshRetryResolveProviderClientVertexNoneWhenNoCredentials(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("VERTEX_PROJECT_ID", "")
	prev := vertexResolveCredentialsPathFn
	vertexResolveCredentialsPathFn = func(context.Context, vertexEnv, string) string { return "" }
	t.Cleanup(func() { vertexResolveCredentialsPathFn = prev })
	client, model, err := ResolveProviderClient(ctx, "vertex", "google/gemini-3-flash-preview", ProviderClientOptions{})
	if err != nil || client != nil || model != "" {
		t.Fatalf("got %v %q %v", client, model, err)
	}
}

func TestAuxiliaryPoolRotationRetryCallLlmRotatesExplicitCodexPoolOn429(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	auxSwapFallbackSeams(t)
	rateErr := auxExc("usage limit reached", 429)
	stale := auxScriptedCallClient("https://chatgpt.com/backend-api/codex", "k", rateErr, rateErr)
	fresh := auxScriptedCallClient("https://chatgpt.com/backend-api/codex", "k", auxCallOK("rotated-sync"))
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "openai-codex", Model: "gpt-5.4"})
	resolves := 0
	prevCached := auxGetCachedClientFn
	auxGetCachedClientFn = func(context.Context, auxCachedClientRequest) (AuxClient, string, error) {
		resolves++
		if resolves == 1 {
			return stale, "gpt-5.4", nil
		}
		return fresh, "gpt-5.4", nil
	}
	t.Cleanup(func() { auxGetCachedClientFn = prevCached })
	prevRefresh := auxRefreshProviderCredentialsFn
	auxRefreshProviderCredentialsFn = func(context.Context, string, string) bool { return false }
	t.Cleanup(func() { auxRefreshProviderCredentialsFn = prevRefresh })
	pool := &auxFakeRecoveryPool{}
	prevPool := auxRecoveryPoolFn
	auxRecoveryPoolFn = func(context.Context, string, string) (auxRecoveryPool, error) { return pool, nil }
	t.Cleanup(func() { auxRecoveryPoolFn = prevPool })
	auxTryPaymentFallbackFn = func(context.Context, string, string, string, string, FailureScope, map[string]any) (AuxClient, string, string, error) {
		t.Error("payment fallback must not run when the pool rotates")
		return nil, "", "", nil
	}
	resp, err := CallLLM(ctx, map[string]any{"task": "compression", "provider": "openai-codex", "model": "gpt-5.4", "messages": auxUserMessages("hi")})
	if err != nil || auxActualContent(t, resp) != "rotated-sync" {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if auxCallCount(stale) != 2 || auxCallCount(fresh) != 1 {
		t.Fatalf("stale calls %d fresh calls %d", auxCallCount(stale), auxCallCount(fresh))
	}
	if len(pool.rotations) != 1 || pool.rotations[0].StatusCode != 429 {
		t.Fatalf("rotations %+v", pool.rotations)
	}
}
