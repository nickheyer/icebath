package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Writes auth.json into the temp home of the context
func auxWriteAuthJSON(t *testing.T, ctx context.Context, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auxAuthJSONPath(ctx), raw, 0o600); err != nil {
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

func TestResolveXaiOAuthForAuxReadsPoolOnlyCredentials(t *testing.T) {
	cases := []struct {
		name, envBase, want string
	}{
		{"uses pool backed credentials without singleton", "", DefaultXAIOAuthBaseURL},
		{"pool backed credentials honor base url env override", "https://example.x.ai/v1/", "https://example.x.ai/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := auxMainTestHome(t, "")
			auxWriteAuthJSON(t, ctx, map[string]any{"version": 1, "providers": map[string]any{}})
			t.Setenv("ICEBATH_XAI_BASE_URL", tc.envBase)
			t.Setenv("XAI_BASE_URL", "")
			pool := LoadPool(modelsPoolEnv(ctx), "xai-oauth")
			if _, err := pool.AddEntry(PooledCredential{Provider: "xai-oauth", ID: "xai123", Label: "pool-only", AuthType: AuthTypeOAuth,
				Source: "manual:xai_pkce", AccessToken: "pool-access-token", RefreshToken: "pool-refresh-token", BaseURL: DefaultXAIOAuthBaseURL}); err != nil {
				t.Fatal(err)
			}
			key, base, ok := auxResolveXaiOAuthForAux(ctx)
			if !ok || key != "pool-access-token" || base != tc.want {
				t.Fatalf("got %q %q %v", key, base, ok)
			}
		})
	}
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

func TestTryOpenrouterPaidLaneGuard(t *testing.T) {
	cases := []struct {
		name      string
		aux       map[string]any
		model     string
		wantModel string
	}{
		{"free only allows builtin default model", map[string]any{"free_only": true}, "", auxOpenRouterModel},
		{"free only skips paid configured model", map[string]any{"free_only": true, "openrouter_model": "google/gemini-3.6-flash"}, "", ""},
		{"free only allows free model", map[string]any{"free_only": true, "openrouter_model": "nvidia/nemotron-3-ultra-550b-a55b:free"}, "",
			"nvidia/nemotron-3-ultra-550b-a55b:free"},
		{"configured model overrides hardcoded default", map[string]any{"openrouter_model": "some/vendor-model"}, "", "some/vendor-model"},
		{"explicit caller model respects free only", map[string]any{"free_only": true}, "google/gemini-3.6-flash", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := auxMainTestHome(t, "")
			t.Setenv("OPENROUTER_API_KEY", "or-key")
			auxFixedPoolEntry(t, false, nil)
			auxFixedConfig(t, map[string]any{"auxiliary": tc.aux})
			client, model, err := auxTryOpenrouter(ctx, nil, tc.model, "")
			if err != nil || model != tc.wantModel || (client == nil) != (tc.wantModel == "") {
				t.Fatalf("got %v %q %v", client, model, err)
			}
		})
	}
}

func TestTryOpenrouterPaidLaneWarnsOnce(t *testing.T) {
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

// Pool fake whose selection is stale until refreshed
type auxStaleNousPool struct {
	stale, fresh PooledCredential
	refreshed    bool
}

func (p *auxStaleNousPool) HasCredentials() bool                   { return true }
func (p *auxStaleNousPool) Select(string) (PooledCredential, bool) { return p.stale, true }
func (p *auxStaleNousPool) TryRefreshCurrent() (PooledCredential, bool) {
	p.refreshed = true
	return p.fresh, true
}

func TestTryNousRefreshesStalePoolEntry(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	entry := func(token string) PooledCredential {
		return PooledCredential{Provider: "nous", AccessToken: "pooled-access-token", AgentKey: token, AgentKeyExpiresAt: "2099-01-01T00:00:00+00:00",
			InferenceBaseURL: "https://inference.pool.example/v1", Extra: map[string]any{"scope": "inference:invoke"}}
	}
	fresh := nousJWT(3600, "inference:invoke", "test-user", nil)
	pool := &auxStaleNousPool{stale: entry(nousJWT(-60, "inference:invoke", "test-user", nil)), fresh: entry(fresh)}
	oldPool, oldRec := auxLoadNousPoolFn, auxGetNousRecommendedAuxModelFn
	t.Cleanup(func() { auxLoadNousPoolFn, auxGetNousRecommendedAuxModelFn = oldPool, oldRec })
	auxLoadNousPoolFn = func(context.Context) auxRefreshablePool { return pool }
	auxGetNousRecommendedAuxModelFn = func(context.Context, bool, bool) string { return "" }
	client, model, err := auxTryNous(ctx, false)
	if err != nil || !pool.refreshed || client == nil || model != auxNousModel {
		t.Fatalf("got %v %q %v refreshed %v", client, model, err, pool.refreshed)
	}
	if client.APIKey() != fresh || strings.TrimRight(client.BaseURL(), "/") != "https://inference.pool.example/v1" {
		t.Fatalf("key %q base %q", client.APIKey(), client.BaseURL())
	}
}

func TestRefreshNousRecommendedModelFallsBackWhenPortalUnavailable(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	old := auxGetNousRecommendedAuxModelFn
	t.Cleanup(func() { auxGetNousRecommendedAuxModelFn = old })
	auxGetNousRecommendedAuxModelFn = func(context.Context, bool, bool) string { panic(errors.New("portal down")) }
	if got := auxRefreshNousRecommendedModel(ctx, false, "some/dead-model"); got != auxNousModel {
		t.Fatalf("got %q", got)
	}
	if got := auxRefreshNousRecommendedModel(ctx, false, auxNousModel); got != "" {
		t.Fatalf("stale default gave %q", got)
	}
}

func TestReadNousAuthFromAuthJSON(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxFixedPoolEntry(t, false, nil)
	if got := auxReadNousAuth(ctx); got != nil {
		t.Fatalf("missing file gave %v", got)
	}
	auxWriteAuthJSON(t, ctx, map[string]any{"active_provider": "openrouter", "providers": map[string]any{"nous": map[string]any{"access_token": "a"}}})
	if got := auxReadNousAuth(ctx); got != nil {
		t.Fatalf("inactive provider gave %v", got)
	}
	auxWriteAuthJSON(t, ctx, map[string]any{"active_provider": "nous", "providers": map[string]any{"nous": map[string]any{"refresh_token": "r"}}})
	if got := auxReadNousAuth(ctx); got != nil {
		t.Fatalf("tokenless state gave %v", got)
	}
	auxWriteAuthJSON(t, ctx, map[string]any{"active_provider": "nous", "providers": map[string]any{"nous": map[string]any{"agent_key": "k", "x": 1.0}}})
	if got := auxReadNousAuth(ctx); got["agent_key"] != "k" || got["x"] != 1.0 {
		t.Fatalf("got %v", got)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(auxAuthJSONPath(ctx)), "auth.json"), []byte("[1]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := auxReadNousAuth(ctx); got != nil {
		t.Fatalf("list payload gave %v", got)
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
	auxMarkProviderUnhealthy(ctx, "Codex", 60, "", "no usable Nous inference JWT", -4)
	if !auxIsProviderUnhealthy(ctx, "openai-codex", "") || auxIsProviderUnhealthy(ctx, "nous", "") {
		t.Fatal("codex alias not hidden or nous hidden")
	}
	auxLogSkipUnhealthy(ctx, "openai-codex", "", "")
	auxLogSkipUnhealthy(ctx, "openai-codex", "", "")
	if got := logs.matching("Auxiliary call: skipping openai-codex (no usable Nous inference JWT, retry in 60s)"); len(got) != 1 {
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
