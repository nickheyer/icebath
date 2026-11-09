package provider

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Temp home whose auth.json holds one Codex access token and no pool
func auxCodexAuthHome(t *testing.T, accessToken string) context.Context {
	t.Helper()
	ctx := auxMainTestHome(t, "")
	auxWriteAuthJSON(t, ctx, map[string]any{"version": 1, "providers": map[string]any{
		"openai-codex": map[string]any{"tokens": map[string]any{"access_token": accessToken, "refresh_token": "r"}},
	}})
	auxFixedPoolEntry(t, false, nil)
	return ctx
}

// Swaps the auth.json Codex token reader for a fixed token
func auxFixedCodexSingletonToken(t *testing.T, token string) {
	t.Helper()
	prev := auxReadCodexSingletonTokenFn
	auxReadCodexSingletonTokenFn = func(context.Context) string { return token }
	t.Cleanup(func() { auxReadCodexSingletonTokenFn = prev })
}

// Swaps the Anthropic SDK builder alone, recording the key and base URL it receives
func auxRecordAnthropicBuild(t *testing.T) *[2]any {
	t.Helper()
	captured := &[2]any{}
	prev := auxBuildAnthropicClientFn
	auxBuildAnthropicClientFn = func(_ context.Context, apiKey any, baseURL string, _ float64, _ bool) (*anthropicSDKClient, error) {
		captured[0], captured[1] = apiKey, baseURL
		return &anthropicSDKClient{}, nil
	}
	t.Cleanup(func() { auxBuildAnthropicClientFn = prev })
	return captured
}

// Oauth flag of the completions adapter inside an auxiliary Anthropic client
func auxAnthropicIsOAuth(t *testing.T, client AuxClient) bool {
	t.Helper()
	wrapped, ok := client.(*AnthropicAuxiliaryClient)
	if !ok {
		t.Fatalf("got %T", client)
	}
	return wrapped.adapter.isOAuth
}

func TestResolveCodexCredentialTokenValidAuthStore(t *testing.T) {
	ctx := auxCodexAuthHome(t, "tok-123")
	token, _, err := auxResolveCodexCredentialAndBase(ctx)
	if err != nil || token != "tok-123" {
		t.Fatalf("got %q %v", token, err)
	}
}

func TestResolveCodexCredentialTokenExpiredJwtReturnsNone(t *testing.T) {
	ctx := auxCodexAuthHome(t, auxCodexJWT(time.Now().Unix()-3600))
	token, _, err := auxResolveCodexCredentialAndBase(ctx)
	if err != nil || token != "" {
		t.Fatalf("Expired JWT should return None, got %q %v", token, err)
	}
}

func TestResolveCodexCredentialTokenValidJwtReturnsToken(t *testing.T) {
	valid := auxCodexJWT(time.Now().Unix() + 3600)
	ctx := auxCodexAuthHome(t, valid)
	token, _, err := auxResolveCodexCredentialAndBase(ctx)
	if err != nil || token != valid {
		t.Fatalf("got %q %v", token, err)
	}
}

func TestAnthropicOAuthFlagOauthTokenSetsFlag(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("ANTHROPIC_TOKEN", "sk-ant-oat01-test-token")
	auxRecordAnthropicBuild(t)
	client, _, err := auxTryAnthropic(ctx, nil, "")
	if err != nil || client == nil {
		t.Fatalf("got %v %v", client, err)
	}
	if !auxAnthropicIsOAuth(t, client) {
		t.Fatal("is_oauth false for an sk-ant-oat01 token")
	}
}

func TestAnthropicOAuthFlagApiKeyNoOauthFlag(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxFakeAnthropicBuild(t, func(context.Context, string) (bool, *PooledCredential) { return false, nil }, "sk-ant-api03-testkey1234")
	client, _, err := auxTryAnthropic(ctx, nil, "")
	if err != nil || client == nil {
		t.Fatalf("got %v %v", client, err)
	}
	if auxAnthropicIsOAuth(t, client) {
		t.Fatal("is_oauth true for an sk-ant-api03 key")
	}
}

func TestAnthropicOAuthFlagPoolEntryTakesPriorityOverLegacyResolution(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	entry := &PooledCredential{Provider: "anthropic", AccessToken: "sk-ant-oat01-pooled", BaseURL: "https://api.anthropic.com"}
	captured := auxFakeAnthropicBuild(t, func(context.Context, string) (bool, *PooledCredential) { return true, entry }, "")
	auxResolveAnthropicTokenFn = func(context.Context) string {
		t.Fatal("legacy path should not run")
		return ""
	}
	client, _, err := auxTryAnthropic(ctx, nil, "")
	if err != nil || client == nil {
		t.Fatalf("got %v %v", client, err)
	}
	if captured[0] != "sk-ant-oat01-pooled" {
		t.Fatalf("built with %v", captured[0])
	}
}

func TestBuildCodexClientPoolWithoutSelectedEntryFallsBackToAuthStore(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxFixedPoolEntry(t, true, nil)
	auxFixedCodexSingletonToken(t, "codex-auth-token")
	client, model, err := auxBuildCodexClient(ctx, "gpt-5.4")
	if err != nil || client == nil || model != "gpt-5.4" {
		t.Fatalf("got %v %q %v", client, model, err)
	}
	if client.APIKey() != "codex-auth-token" || strings.TrimRight(client.BaseURL(), "/") != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("key %q base %q", client.APIKey(), client.BaseURL())
	}
}

func TestBuildCodexClientProfileCodexBaseUrlOverridesPoolEndpoint(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	entry := &PooledCredential{Provider: "openai-codex", AccessToken: "codex-pool-token", BaseURL: "https://chatgpt.com/backend-api/codex"}
	auxFixedPoolEntry(t, true, entry)
	t.Setenv("ICEBATH_CODEX_BASE_URL", "http://127.0.0.1:8787/v1")
	client, model, err := auxBuildCodexClient(ctx, "gpt-5.4")
	if err != nil || client == nil || model != "gpt-5.4" {
		t.Fatalf("got %v %q %v", client, model, err)
	}
	if strings.TrimRight(client.BaseURL(), "/") != "http://127.0.0.1:8787/v1" {
		t.Fatalf("base %q", client.BaseURL())
	}
}

func TestBuildCodexClientProfileCodexBaseUrlAppliesToRawCodexClient(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxFixedPoolEntry(t, false, nil)
	auxFixedCodexSingletonToken(t, "codex-auth-token")
	t.Setenv("ICEBATH_CODEX_BASE_URL", "http://127.0.0.1:8787/v1")
	client, model, err := ResolveProviderClient(ctx, "openai-codex", "gpt-5.4", ProviderClientOptions{RawCodex: true})
	if err != nil || client == nil || model != "gpt-5.4" {
		t.Fatalf("got %v %q %v", client, model, err)
	}
	if strings.TrimRight(client.BaseURL(), "/") != "http://127.0.0.1:8787/v1" {
		t.Fatalf("base %q", client.BaseURL())
	}
}

func TestBuildCodexClientRejectsMissingModel(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	client, model, err := auxBuildCodexClient(ctx, "")
	if err != nil || client != nil || model != "" {
		t.Fatalf("got %v %q %v", client, model, err)
	}
}

func TestBuildCodexClientCachedCodexClientRebuildsWhenPoolEntryChanges(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxIsolateClientCache(t)
	t.Setenv("ICEBATH_CODEX_BASE_URL", "")
	add := func(id, token string) {
		pool := LoadPool(modelsPoolEnv(ctx), "openai-codex")
		for pool.EntryCount() > 0 {
			if _, ok := pool.RemoveIndex(1); !ok {
				t.Fatal("remove failed")
			}
		}
		if _, err := pool.AddEntry(PooledCredential{Provider: "openai-codex", ID: id, Label: id, AuthType: AuthTypeOAuth,
			Source: "manual:codex", AccessToken: token, RefreshToken: "r", BaseURL: "https://chatgpt.com/backend-api/codex"}); err != nil {
			t.Fatal(err)
		}
	}
	add("cred-a", "tok-a")
	ShutdownCachedClients(ctx)
	t.Cleanup(func() { ShutdownCachedClients(ctx) })
	first, firstModel, err := auxGetCachedClient(ctx, auxCachedClientRequest{Provider: "openai-codex", Model: "gpt-5.4"})
	if err != nil || first == nil {
		t.Fatalf("first %v %v", first, err)
	}
	add("cred-b", "tok-b")
	second, secondModel, err := auxGetCachedClient(ctx, auxCachedClientRequest{Provider: "openai-codex", Model: "gpt-5.4"})
	if err != nil || second == nil {
		t.Fatalf("second %v %v", second, err)
	}
	if first == second || firstModel != "gpt-5.4" || secondModel != "gpt-5.4" {
		t.Fatalf("same client %v models %q %q", first == second, firstModel, secondModel)
	}
	if first.APIKey() != "tok-a" || second.APIKey() != "tok-b" {
		t.Fatalf("keys %q %q", first.APIKey(), second.APIKey())
	}
}

func TestResolveProviderClientUniversalModelFallbackEmptyModelForCodexAlsoUsesMainModel(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	prevMain, prevAux, prevBuild := auxReadMainModel, auxGetAuxModelForProviderFn, auxBuildCodexClientFn
	t.Cleanup(func() {
		auxReadMainModel, auxGetAuxModelForProviderFn, auxBuildCodexClientFn = prevMain, prevAux, prevBuild
	})
	auxReadMainModel = func(context.Context) string { return "gpt-5.4" }
	auxGetAuxModelForProviderFn = func(context.Context, string) string { return "" }
	built := ""
	auxBuildCodexClientFn = func(_ context.Context, model string) (AuxClient, string, error) {
		built = model
		return &auxCallFakeClient{}, "gpt-5.4", nil
	}
	auxFixedPoolEntry(t, true, nil)
	client, model, err := ResolveProviderClient(ctx, "openai-codex", "", ProviderClientOptions{})
	if err != nil || client == nil || model != "gpt-5.4" || built != "gpt-5.4" {
		t.Fatalf("got %v %q built %q %v", client, model, built, err)
	}
}

func TestResolveProviderClientUniversalModelFallbackExplicitModelTakesPrecedenceOverFallbacks(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	prevMain, prevAux, prevBuild := auxReadMainModel, auxGetAuxModelForProviderFn, auxBuildXaiOAuthAuxClientFn
	t.Cleanup(func() {
		auxReadMainModel, auxGetAuxModelForProviderFn, auxBuildXaiOAuthAuxClientFn = prevMain, prevAux, prevBuild
	})
	mainReads := 0
	auxReadMainModel = func(context.Context) string { mainReads++; return "" }
	auxGetAuxModelForProviderFn = func(context.Context, string) string { return "catalog-default-should-not-be-used" }
	built := ""
	auxBuildXaiOAuthAuxClientFn = func(_ context.Context, model string) (AuxClient, string, error) {
		built = model
		return &auxCallFakeClient{}, "grok-4.20-multi-agent", nil
	}
	client, model, err := ResolveProviderClient(ctx, "xai-oauth", "grok-4.20-multi-agent", ProviderClientOptions{})
	if err != nil || client == nil || model != "grok-4.20-multi-agent" || built != "grok-4.20-multi-agent" {
		t.Fatalf("got %v %q built %q %v", client, model, built, err)
	}
	if mainReads != 0 {
		t.Fatalf("main model read %d times", mainReads)
	}
}

func TestExpiredCodexFallbackClaudeCodeOauthEnvSetsFlag(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-cc-test-token")
	t.Setenv("ANTHROPIC_TOKEN", "")
	auxRecordAnthropicBuild(t)
	client, _, err := auxTryAnthropic(ctx, nil, "")
	if err != nil || client == nil {
		t.Fatalf("got %v %v", client, err)
	}
	if !auxAnthropicIsOAuth(t, client) {
		t.Fatal("is_oauth false for CLAUDE_CODE_OAUTH_TOKEN")
	}
}

func TestExplicitProviderRoutingExplicitAnthropicApiKey(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxFakeAnthropicBuild(t, func(context.Context, string) (bool, *PooledCredential) { return false, nil }, "sk-ant-api-regular-key")
	client, _, err := ResolveProviderClient(ctx, "anthropic", "", ProviderClientOptions{})
	if err != nil || client == nil {
		t.Fatalf("got %v %v", client, err)
	}
	if auxAnthropicIsOAuth(t, client) {
		t.Fatal("is_oauth true for a regular key")
	}
}

// Swaps the api key chain registry rows for one profile and clears the env keys that would configure it
func auxSingleRegistryRow(t *testing.T, row *Profile) {
	t.Helper()
	prev := auxRegistryRowsFn
	auxRegistryRowsFn = func(context.Context) []*Profile { return []*Profile{row} }
	t.Cleanup(func() { auxRegistryRowsFn = prev })
	for _, name := range row.KeyEnv {
		t.Setenv(name, "")
	}
}

func TestResolveApiKeyProviderSkipsUnconfiguredAnthropic(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	auxSingleRegistryRow(t, &Profile{Name: "anthropic", DisplayName: "Anthropic", AuthType: AuthTypeAPIKey,
		BaseURL: "https://api.anthropic.com", KeyEnv: []string{"ANTHROPIC_API_KEY"}})
	t.Setenv("ANTHROPIC_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	var called []string
	auxFakeAnthropicBuild(t, func(context.Context, string) (bool, *PooledCredential) {
		called = append(called, "anthropic")
		return false, nil
	}, "")
	if _, _, err := auxResolveAPIKeyProvider(ctx); err != nil {
		t.Fatal(err)
	}
	if len(called) != 0 {
		t.Fatal("_try_anthropic() should not be called when anthropic is not explicitly configured")
	}
}

func TestResolveApiKeyProviderSkipsUnconfiguredCopilot(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	auxSingleRegistryRow(t, &Profile{Name: "copilot", DisplayName: "Copilot", AuthType: AuthTypeAPIKey,
		BaseURL: "https://api.githubcopilot.com", KeyEnv: []string{"GITHUB_COPILOT_TOKEN", "GH_TOKEN"}})
	var selected []string
	prev := auxSelectPoolEntryFn
	auxSelectPoolEntryFn = func(_ context.Context, provider string) (bool, *PooledCredential) {
		selected = append(selected, provider)
		return false, nil
	}
	t.Cleanup(func() { auxSelectPoolEntryFn = prev })
	client, model, err := auxResolveAPIKeyProvider(ctx)
	if err != nil || client != nil || model != "" {
		t.Fatalf("got %v %q %v", client, model, err)
	}
	for _, provider := range selected {
		if provider == "copilot" {
			t.Fatal("_select_pool_entry() should not be called for unconfigured copilot")
		}
	}
}

func TestPoolRuntimeBaseUrlUsesNousEnvOverride(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	entry := &PooledCredential{Provider: "nous", InferenceBaseURL: "https://inference-api.nousresearch.com/v1", BaseURL: "https://inference-api.nousresearch.com/v1"}
	t.Setenv("NOUS_INFERENCE_BASE_URL", "https://ai.wildebeest-newton.ts.net/v1")
	if got := auxPoolRuntimeBaseURL(ctx, entry, ""); got != "https://ai.wildebeest-newton.ts.net/v1" {
		t.Fatalf("got %q", got)
	}
}
