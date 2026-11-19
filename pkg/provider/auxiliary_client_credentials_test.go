package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Writes auth.json into the temp home of the context
func auxWriteAuthJSON(t *testing.T, ctx context.Context, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home.Dir(ctx), "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Unsigned JWT whose payload carries the exp claim
func auxCodexJWT(exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp": %d}`, exp)))
	return header + "." + payload + ".fakesig"
}

// Swaps the read only config seam for a fixed tree
func auxFixedConfig(t *testing.T, tree map[string]any) {
	t.Helper()
	old := auxLoadConfigReadonlyFn
	t.Cleanup(func() { auxLoadConfigReadonlyFn = old })
	auxLoadConfigReadonlyFn = func(context.Context) (config.Tree, error) { return config.Tree(tree), nil }
}

// Swaps the pool selector seam for a fixed answer
func auxFixedPoolEntry(t *testing.T, present bool, entry *PooledCredential) {
	t.Helper()
	old := auxSelectPoolEntryFn
	t.Cleanup(func() { auxSelectPoolEntryFn = old })
	auxSelectPoolEntryFn = func(context.Context, string) (bool, *PooledCredential) { return present, entry }
}

func TestNormalizeAuxProviderCoversEveryAuthAlias(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	for alias, canonical := range authProviderAliases {
		if got := auxNormalizeAuxProvider(ctx, alias); got != canonical {
			t.Errorf("%s: got %q want %q", alias, got, canonical)
		}
	}
}

func TestResolveCodexCredentialTokenFromAuthStore(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name, token, want string
	}{
		{"valid auth store", "tok-123", "tok-123"},
		{"expired jwt returns none", auxCodexJWT(now - 3600), ""},
		{"valid jwt returns token", auxCodexJWT(now + 3600), auxCodexJWT(now + 3600)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := auxMainTestHome(t, "")
			auxWriteAuthJSON(t, ctx, map[string]any{"version": 1, "providers": map[string]any{
				"openai-codex": map[string]any{"tokens": map[string]any{"access_token": tc.token, "refresh_token": "r-456"}}}})
			auxFixedPoolEntry(t, false, nil)
			token, base, err := auxResolveCodexCredentialAndBase(ctx)
			if err != nil || token != tc.want || base != DefaultCodexBaseURL {
				t.Fatalf("got %q %q %v", token, base, err)
			}
		})
	}
}

// Pool only xAI OAuth credentials under an env base URL override, the expected base URL asserted
func auxXaiPoolOnlyCase(t *testing.T, envBase, want string) {
	t.Helper()
	ctx := auxMainTestHome(t, "")
	auxWriteAuthJSON(t, ctx, map[string]any{"version": 1, "providers": map[string]any{}})
	t.Setenv("ICEBATH_XAI_BASE_URL", envBase)
	t.Setenv("XAI_BASE_URL", "")
	pool := LoadPool(modelsPoolEnv(ctx), "xai-oauth")
	if _, err := pool.AddEntry(PooledCredential{Provider: "xai-oauth", ID: "xai123", Label: "pool-only", AuthType: AuthTypeOAuth,
		Source: "manual:xai_pkce", AccessToken: "pool-access-token", RefreshToken: "pool-refresh-token", BaseURL: DefaultXAIOAuthBaseURL}); err != nil {
		t.Fatal(err)
	}
	key, base, ok := auxResolveXaiOAuthForAux(ctx)
	if !ok || key != "pool-access-token" || base != want {
		t.Fatalf("got %q %q %v", key, base, ok)
	}
}

func TestResolveXaiOAuthForAuxReadsPoolOnlyCredentials(t *testing.T) {
	t.Run("test_uses_pool_backed_credentials_without_singleton", func(t *testing.T) { auxXaiPoolOnlyCase(t, "", DefaultXAIOAuthBaseURL) })
	t.Run("test_pool_backed_credentials_honor_base_url_env_override", func(t *testing.T) {
		auxXaiPoolOnlyCase(t, "https://example.x.ai/v1/", "https://example.x.ai/v1")
	})
}

func TestTryOpenrouterPoolExhaustedFallsBackToEnv(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-env-fallback")
	auxFixedPoolEntry(t, true, nil)
	client, model, err := auxTryOpenrouter(ctx, nil, "", "")
	if err != nil || client == nil || model != auxOpenRouterModel {
		t.Fatalf("got %v %q %v", client, model, err)
	}
	if client.APIKey() != "sk-or-env-fallback" || strings.TrimRight(client.BaseURL(), "/") != OpenRouterBaseURL {
		t.Fatalf("key %q base %q", client.APIKey(), client.BaseURL())
	}
}

// Runs the OpenRouter lane under a pool free home, an env key and the given auxiliary config
func auxPaidLaneCase(t *testing.T, aux map[string]any, model, wantModel string) {
	t.Helper()
	ctx := auxMainTestHome(t, "")
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	auxFixedPoolEntry(t, false, nil)
	auxFixedConfig(t, map[string]any{"auxiliary": aux})
	client, got, err := auxTryOpenrouter(ctx, nil, model, "")
	if err != nil || got != wantModel || (client == nil) != (wantModel == "") {
		t.Fatalf("got %v %q %v", client, got, err)
	}
}

func TestOpenRouterPaidLaneGuardFreeOnlyAllowsBuiltinDefaultModel(t *testing.T) {
	auxPaidLaneCase(t, map[string]any{"free_only": true}, "", auxOpenRouterModel)
}

func TestOpenRouterPaidLaneGuardFreeOnlySkipsPaidConfiguredModel(t *testing.T) {
	auxPaidLaneCase(t, map[string]any{"free_only": true, "openrouter_model": "google/gemini-3.6-flash"}, "", "")
}

func TestOpenRouterPaidLaneGuardFreeOnlyAllowsFreeModel(t *testing.T) {
	auxPaidLaneCase(t, map[string]any{"free_only": true, "openrouter_model": "nvidia/nemotron-3-ultra-550b-a55b:free"}, "", "nvidia/nemotron-3-ultra-550b-a55b:free")
}

func TestOpenRouterPaidLaneGuardConfiguredModelOverridesHardcodedDefault(t *testing.T) {
	auxPaidLaneCase(t, map[string]any{"openrouter_model": "some/vendor-model"}, "", "some/vendor-model")
}

func TestOpenRouterPaidLaneGuardExplicitCallerModelRespectsFreeOnly(t *testing.T) {
	auxPaidLaneCase(t, map[string]any{"free_only": true}, "google/gemini-3.6-flash", "")
}

func TestOpenRouterPaidLaneGuardResolverForwardsExplicitFreeModelToGate(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	auxFixedPoolEntry(t, false, nil)
	auxFixedConfig(t, map[string]any{"auxiliary": map[string]any{"free_only": true}})
	client, model, err := ResolveProviderClient(ctx, "openrouter", "nvidia/nemotron-3-ultra-550b-a55b:free", ProviderClientOptions{})
	if err != nil || client == nil || model != "nvidia/nemotron-3-ultra-550b-a55b:free" {
		t.Fatalf("got %v %q %v", client, model, err)
	}
}

func TestOpenRouterPaidLaneGuardFreeOnlyGateDoesNotMarkOpenrouterUnhealthy(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	auxFixedPoolEntry(t, false, nil)
	auxFixedConfig(t, map[string]any{"auxiliary": map[string]any{"free_only": true}})
	client, model, err := ResolveProviderClient(ctx, "openrouter", "google/gemini-3.6-flash", ProviderClientOptions{})
	if err != nil || client != nil || model != "" {
		t.Fatalf("got %v %q %v", client, model, err)
	}
	if auxIsProviderUnhealthy(ctx, "openrouter", "") {
		t.Fatal("free_only gate marked openrouter unhealthy")
	}
}

func TestOpenRouterPaidLaneGuardIsFreeModel(t *testing.T) {
	for model, want := range map[string]bool{"nvidia/nemotron-3-ultra-550b-a55b:free": true, "stealth/ox-alpha": true,
		"google/gemini-3.6-flash": false, "my-stealth/model": false, "": false} {
		if auxIsFreeModel(model) != want {
			t.Fatalf("%q free %v", model, !want)
		}
	}
}

func TestOpenRouterPaidLaneGuardPaidLaneWarnsOnce(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	paid := "google/gemini-3.6-flash"
	auxPaidLaneWarnedMu.Lock()
	delete(auxPaidLaneWarned, paid)
	auxPaidLaneWarnedMu.Unlock()
	t.Cleanup(func() {
		auxPaidLaneWarnedMu.Lock()
		delete(auxPaidLaneWarned, paid)
		auxPaidLaneWarnedMu.Unlock()
	})
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	auxFixedPoolEntry(t, false, nil)
	auxFixedConfig(t, map[string]any{"auxiliary": map[string]any{"openrouter_model": paid}})
	logs := auxCaptureLogs(t)
	client, model, err := auxTryOpenrouter(ctx, nil, "", "")
	if err != nil || client == nil || model != paid || len(logs.matching("PAID lane engaged")) != 1 {
		t.Fatalf("got %v %q %v logs %d", client, model, err, len(logs.matching("PAID lane engaged")))
	}
	if _, _, err := auxTryOpenrouter(ctx, nil, "", ""); err != nil || len(logs.matching("PAID lane engaged")) != 1 {
		t.Fatalf("second call warned again: %v", err)
	}
}

func TestUnhealthyCacheExpiresAndLogsSkipOncePerMinute(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxResetAuxUnhealthyCache()
	t.Cleanup(auxResetAuxUnhealthyCache)
	now := 1000.0
	old := auxUnhealthyNow
	t.Cleanup(func() { auxUnhealthyNow = old })
	auxUnhealthyNow = func() float64 { return now }
	logs := auxCaptureLogs(t)
	auxMarkProviderUnhealthy(ctx, "Codex", 60, "", "no usable access token", -4)
	if !auxIsProviderUnhealthy(ctx, "openai-codex", "") || auxIsProviderUnhealthy(ctx, "openrouter", "") {
		t.Fatal("codex alias not hidden or openrouter hidden")
	}
	auxLogSkipUnhealthy(ctx, "openai-codex", "", "")
	auxLogSkipUnhealthy(ctx, "openai-codex", "", "")
	if got := logs.matching("Auxiliary call: skipping openai-codex (no usable access token, retry in 60s)"); len(got) != 1 {
		t.Fatalf("skip logs %d", len(got))
	}
	now += 60
	if auxIsProviderUnhealthy(ctx, "openai-codex", "") {
		t.Fatal("expired entry still hidden")
	}
	if auxNormalizeChainLabel(" Custom ") != "local/custom" || auxNormalizeChainLabel("ZAI") != "zai" || auxNormalizeChainLabel("") != "" {
		t.Fatal("chain label mapping")
	}
}

func TestResolveCodexCredentialAndBaseReadsTheAuthStore(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxWriteAuthJSON(t, ctx, map[string]any{"version": 1, "providers": map[string]any{
		"openai-codex": map[string]any{"tokens": map[string]any{"access_token": "tok-123", "refresh_token": "r-456"}}}})
	auxFixedPoolEntry(t, false, nil)
	token, base, err := ResolveCodexCredentialAndBase(ctx)
	if err != nil || token != "tok-123" || base != DefaultCodexBaseURL {
		t.Fatalf("got %q %q %v", token, base, err)
	}
}
