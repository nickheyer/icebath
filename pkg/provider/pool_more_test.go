package provider

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nickheyer/icebath/pkg/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- test_credential_pool_provider_boundary.py ----

func customCfg(entries ...map[string]any) map[string]any {
	list := make([]any, 0, len(entries))
	providers := map[string]any{}
	for _, e := range entries {
		if key, ok := e["provider_key"].(string); ok {
			providers[key] = map[string]any{"name": e["name"], "api": e["base_url"]}
			continue
		}
		list = append(list, e)
	}
	return map[string]any{"custom_providers": list, "providers": providers}
}

func TestProviderMatchRequiresExactNonCustomIdentity(t *testing.T) {
	cfg := map[string]any{}
	if !CredentialPoolMatchesProviderIn("deepseek", "deepseek", "", cfg) || CredentialPoolMatchesProviderIn("openai-codex", "deepseek", "", cfg) || CredentialPoolMatchesProviderIn("", "deepseek", "", cfg) {
		t.Fatal("non custom identities must match exactly and fail closed on empty")
	}
}

func TestCustomPoolMatchIsScopedByEndpoint(t *testing.T) {
	cfg := customCfg(map[string]any{"name": "lab", "base_url": "https://lab.example/v1"})
	if !CredentialPoolMatchesProviderIn("custom:lab", "custom", "https://lab.example/v1", cfg) {
		t.Fatal("the configured endpoint must match its pool")
	}
	if CredentialPoolMatchesProviderIn("custom:other", "custom", "https://lab.example/v1", cfg) {
		t.Fatal("another pool must not serve the endpoint")
	}
}

func TestNamedCustomPoolMatchRequiresConfiguredIdentityAndEndpoint(t *testing.T) {
	cfg := customCfg(map[string]any{"name": "Gemini Display", "provider_key": "gemini-no-filter", "base_url": "https://generativelanguage.googleapis.com/v1beta/"})
	endpoint := "https://generativelanguage.googleapis.com/v1beta"
	if !CredentialPoolMatchesProviderIn("custom:gemini-display", "gemini-no-filter", endpoint, cfg) || !CredentialPoolMatchesProviderIn("custom:gemini-display", "custom:gemini-no-filter", endpoint, cfg) {
		t.Fatal("configured aliases at the configured endpoint must match")
	}
	for _, c := range []struct{ provider, url string }{{"gemini-no-filter", "https://fallback.example/v1"}, {"custom:gemini-no-filter", "https://fallback.example/v1"}, {"other-provider", endpoint}} {
		if CredentialPoolMatchesProviderIn("custom:gemini-display", c.provider, c.url, cfg) {
			t.Fatalf("%s at %s must not match", c.provider, c.url)
		}
	}
}

func TestRuntimePoolKeyResolvesAllCustomRuntimeIdentities(t *testing.T) {
	endpoint := "https://generativelanguage.googleapis.com/v1beta"
	cfg := customCfg(map[string]any{"name": "Sibling Display", "provider_key": "sibling-provider", "base_url": endpoint},
		map[string]any{"name": "Gemini Display", "provider_key": "gemini-no-filter", "base_url": endpoint})
	firstOwner := customProviderPoolKeyCandidates(endpoint, "", cfg)[0]
	if got := ResolveRuntimePoolKey("custom", endpoint, cfg); got != firstOwner || got != "gemini-no-filter" {
		t.Fatalf("bare custom resolves the config view's first endpoint owner %q, got %q", firstOwner, got)
	}
	if got := ResolveRuntimePoolKey("gemini-no-filter", endpoint, cfg); got != "gemini-no-filter" {
		t.Fatalf("named identity keys on its own slug, got %q", got)
	}
	if got := ResolveRuntimePoolKey("custom:gemini-no-filter", endpoint, cfg); got != "gemini-no-filter" {
		t.Fatalf("custom prefixed identity keys on its slug, got %q", got)
	}
	if got := ResolveRuntimePoolKey("gemini-no-filter", "https://fallback.example/v1", cfg); got != "gemini-no-filter" {
		t.Fatalf("a foreign endpoint keeps the normalized identity, got %q", got)
	}
}

func TestRuntimePoolKeyResolvesModernProviderInMixedConfig(t *testing.T) {
	endpoint := "https://generativelanguage.googleapis.com/v1beta"
	cfg := map[string]any{
		"custom_providers": []any{map[string]any{"name": "Legacy Provider", "base_url": "https://legacy.example/v1"}},
		"providers":        map[string]any{"gemini-no-filter": map[string]any{"name": "Gemini Display", "api": endpoint}},
	}
	if got := ResolveRuntimePoolKey("gemini-no-filter", endpoint, cfg); got != "gemini-no-filter" {
		t.Fatalf("got %q", got)
	}
	if got := ResolveRuntimePoolKey("custom:gemini-no-filter", endpoint, cfg); got != "gemini-no-filter" {
		t.Fatalf("got %q", got)
	}
	if got := ResolveRuntimePoolKey("custom:gemini-no-filter", "https://fallback.example/v1", cfg); got != "custom:gemini-no-filter" {
		t.Fatalf("a foreign endpoint keeps the custom identity, got %q", got)
	}
}

func TestKeyedProviderPoolMatchesRuntimeAliases(t *testing.T) {
	cfg := customCfg(map[string]any{"name": "B.AI", "provider_key": "b-ai", "base_url": "https://api.b.ai/v1"})
	for _, provider := range []string{"b-ai", "custom", "custom:b.ai"} {
		if !CredentialPoolMatchesProviderIn("b-ai", provider, "https://api.b.ai/v1", cfg) {
			t.Fatalf("%s must match the keyed pool", provider)
		}
	}
	if CredentialPoolMatchesProviderIn("b-ai", "custom", "https://other.example/v1", cfg) || CredentialPoolMatchesProviderIn("b-ai", "deepseek", "https://api.b.ai/v1", cfg) {
		t.Fatal("other endpoints and providers must not match")
	}
}

func TestRuntimePoolKeyPrefersDurableProviderSlug(t *testing.T) {
	endpoint := "https://api.b.ai/v1"
	cfg := customCfg(map[string]any{"name": "B.AI", "provider_key": "b-ai", "base_url": endpoint})
	for _, provider := range []string{"b-ai", "custom", "custom:b.ai"} {
		if got := ResolveRuntimePoolKey(provider, endpoint, cfg); got != "b-ai" {
			t.Fatalf("%s resolves to %q", provider, got)
		}
	}
}

func TestRuntimePoolKeyPreservesNonCustomIdentity(t *testing.T) {
	if got := ResolveRuntimePoolKey("openai-codex", "https://chatgpt.com/backend-api", map[string]any{}); got != "openai-codex" {
		t.Fatalf("got %q", got)
	}
}

func TestPoolRotateIgnoresRouteOfAnotherProvider(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, authWithPool("openai-codex", codexDeviceEntry(1, "tok-a", "rf-a")))
	pool := LoadPool(testEnv(t, h), "openai-codex")
	route := Route{Provider: "deepseek", Model: "deepseek-chat", BaseURL: "https://api.deepseek.com/v1", APIKey: "deepseek-key"}
	if _, ok := pool.Rotate(context.Background(), route, &APIError{Provider: "deepseek", StatusCode: 429, Message: "rate limited"}); ok {
		t.Fatal("a pool must never act on another provider's failure")
	}
	if statusesOf(pool)["codex-1"] == StatusExhausted {
		t.Fatal("the foreign failure must not bench the pool")
	}
}

func TestPoolRotateRebindsRouteToNextEntry(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, authWithPool("openrouter",
		row(map[string]any{"id": "k1", "label": "one", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-or-one", "base_url": "https://openrouter.ai/api/v1"}),
		row(map[string]any{"id": "k2", "label": "two", "auth_type": "api_key", "priority": 1, "source": "manual", "access_token": "sk-or-two", "base_url": "https://openrouter.ai/api/v1"})))
	pool := LoadPool(testEnv(t, h), "openrouter")
	mustSelect(t, pool, "")
	route := Route{Provider: "openrouter", Model: "m", BaseURL: "https://openrouter.ai/api/v1", APIKey: "sk-or-one", Mode: ModeChatCompletions}
	next, ok := pool.Rotate(context.Background(), route, &APIError{Provider: "openrouter", StatusCode: 402, Message: "Insufficient credits", Body: `{"error":{"message":"Insufficient credits","code":402}}`})
	if !ok || next.APIKey != "sk-or-two" || next.BaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("rotation must rebind the route, got %+v %t", next, ok)
	}
	if statusesOf(pool)["k1"] != StatusExhausted {
		t.Fatal("the failed key must be benched")
	}
}

// ---- test_credential_pool_plugin_seam.py ----

func pluginEntry() PooledCredential {
	return PooledCredential{Provider: "example-oauth", ID: "abc123", Label: "acme", AuthType: AuthTypeOAuth, Priority: 0, Source: "manual:example_device",
		AccessToken: "tok-1", RefreshToken: "rt-1", Extra: map[string]any{"tenant": "acme", "region": "eu"}}
}

func TestPluginMetadataSurvivesLoadSaveLoad(t *testing.T) {
	payload := pluginEntry().ToMap()
	again := PooledCredentialFromMap("example-oauth", payload).ToMap()
	if again["tenant"] != "acme" || again["region"] != "eu" {
		t.Fatalf("plugin metadata lost: %v", again)
	}
	extra := PooledCredentialFromMap("example-oauth", again).Extra
	if len(extra) != 2 || extra["tenant"] != "acme" || extra["region"] != "eu" {
		t.Fatalf("extra must round trip exactly: %v", extra)
	}
	if PooledCredentialFromMap("nous", map[string]any{"access_token": "t", "org_id": "o1"}).ExtraString("org_id") != "o1" {
		t.Fatal("core known extra keys keep their attribute surface")
	}
	withProvider := map[string]any{}
	for k, v := range payload {
		withProvider[k] = v
	}
	withProvider["provider"] = "other"
	if _, has := PooledCredentialFromMap("example-oauth", withProvider).ToMap()["provider"]; has {
		t.Fatal("a stray provider key must not be swept into extra")
	}
}

func registerExamplePlugin(t *testing.T, hook func(PooledCredential) (map[string]any, error)) {
	t.Helper()
	registerHookedProfile(t, "example-oauth", ProviderProfileHooks{RefreshCredential: hook})
	RegisterProvider(Profile{Name: "example-oauth-nohook", AuthType: AuthTypeOAuthExternal, BaseURL: "https://example.invalid/v1"})
}

func TestPoolRefreshDispatchesToProfileHook(t *testing.T) {
	h := newTestHome(t)
	var seen []string
	registerExamplePlugin(t, func(entry PooledCredential) (map[string]any, error) {
		seen = append(seen, entry.RefreshToken)
		return map[string]any{"access_token": "tok-2", "refresh_token": "rt-2"}, nil
	})
	if !IsRefreshableOAuthProvider("example-oauth") || IsRefreshableOAuthProvider("example-oauth-nohook") || !IsRefreshableOAuthProvider("anthropic") {
		t.Fatal("refreshability derives from the hook")
	}
	entry := pluginEntry()
	pool := NewPool(testEnv(t, h), "example-oauth", []PooledCredential{entry})
	refreshed, ok := pool.refreshEntryImpl(entry, true, "", false)
	if !ok || len(seen) != 1 || seen[0] != "rt-1" {
		t.Fatalf("hook must receive the row: %v", seen)
	}
	if refreshed.AccessToken != "tok-2" || refreshed.RefreshToken != "rt-2" || refreshed.Extra["tenant"] != "acme" || refreshed.Extra["region"] != "eu" {
		t.Fatalf("rotated fields must replace and extras survive: %+v", refreshed)
	}
	nohook := pluginEntry()
	nohook.Provider = "example-oauth-nohook"
	unchanged, ok := NewPool(testEnv(t, h), "example-oauth-nohook", []PooledCredential{nohook}).refreshEntryImpl(nohook, true, "", false)
	if !ok || unchanged.AccessToken != "tok-1" || unchanged.LastStatus != "" {
		t.Fatalf("without the hook the row is returned unchanged: %+v", unchanged)
	}
}

func TestPluginRefreshOutcome(t *testing.T) {
	cases := []struct {
		name     string
		hook     func(PooledCredential) (map[string]any, error)
		status   string
		tokens   [2]string
		extraKey string
	}{
		{"token-endpoint-shape-rotates", func(PooledCredential) (map[string]any, error) {
			return map[string]any{"access_token": "tok-2", "refresh_token": "rt-2", "expires_in": 3600, "expires_at_ms": 4102444800000, "token_type": "Bearer"}, nil
		}, StatusOK, [2]string{"tok-2", "rt-2"}, "expires_in"},
		{"transient-error-benches", func(PooledCredential) (map[string]any, error) { return nil, errors.New("token endpoint said 503") }, StatusExhausted, [2]string{"tok-1", "rt-1"}, ""},
		{"relogin-required-is-terminal", func(PooledCredential) (map[string]any, error) {
			return nil, &AuthError{Message: "invalid_grant", Provider: "example-oauth", Code: "example_refresh_failed", ReloginRequired: true}
		}, StatusDead, [2]string{"tok-1", "rt-1"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
			registerExamplePlugin(t, tc.hook)
			sink := &recordingHandler{}
			env := testEnv(t, h)
			env.Log = newRecordingLogger(sink)
			entry := pluginEntry()
			entry.ExpiresAtMS = 1
			pool := NewPool(env, "example-oauth", []PooledCredential{entry})
			pool.persist(false, nil, nil)
			pool.refreshEntry(entry, true, "", false)
			rows, _ := env.Store.ReadCredentialPool("example-oauth")
			rowEntry := PooledCredentialFromMap("example-oauth", rows[0])
			if rowEntry.LastStatus != tc.status || rowEntry.AccessToken != tc.tokens[0] || rowEntry.RefreshToken != tc.tokens[1] {
				t.Fatalf("got status=%s tokens=%s/%s", rowEntry.LastStatus, rowEntry.AccessToken, rowEntry.RefreshToken)
			}
			if tc.extraKey != "" {
				if n, _ := anyInt(rowEntry.Extra[tc.extraKey]); n != 3600 || rowEntry.ExpiresAtMS != 4102444800000 {
					t.Fatalf("token endpoint shape must land in extras and fields: %+v", rowEntry)
				}
			}
			hints := sink.containing("icebath auth add example-oauth")
			if tc.status == StatusDead && len(hints) == 0 {
				t.Fatal("a terminal refresh must name the re-auth command")
			}
			if tc.status != StatusDead && len(sink.containing("icebath auth add")) > 0 {
				t.Fatal("non terminal outcomes must not print the re-auth hint")
			}
		})
	}
}

func TestPluginRefreshAdoptsPeerRotationWithoutSpendingToken(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	var calls []string
	registerExamplePlugin(t, func(entry PooledCredential) (map[string]any, error) {
		calls = append(calls, entry.RefreshToken)
		n := len(calls) + 1
		return map[string]any{"access_token": "tok-" + string(rune('0'+n)), "refresh_token": "rt-" + string(rune('0'+n))}, nil
	})
	env := testEnv(t, h)
	stale := func() PooledCredential {
		e := pluginEntry()
		e.ExpiresAtMS = 1
		return e
	}
	first := NewPool(env, "example-oauth", []PooledCredential{stale()})
	first.persist(false, nil, nil)
	second := NewPool(env, "example-oauth", []PooledCredential{stale()})
	refreshed, ok := first.refreshEntry(first.Entries()[0], true, "", false)
	if !ok || refreshed.AccessToken != "tok-2" {
		t.Fatalf("first refresh rotates: %+v", refreshed)
	}
	adopted, ok := second.refreshEntry(second.Entries()[0], true, "", false)
	if !ok || adopted.AccessToken != "tok-2" || adopted.RefreshToken != "rt-2" {
		t.Fatalf("second refresh adopts the peer rotation: %+v", adopted)
	}
	if len(calls) != 1 || calls[0] != "rt-1" {
		t.Fatalf("the single use token must be spent once: %v", calls)
	}
}

// ---- test_credential_pool_oauth_writethrough.py ----

func profileAndRoot(t *testing.T) (string, string) {
	t.Helper()
	root := newTestHome(t)
	profile := filepath.Join(root, "profiles", "work")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	return profile, root
}

func oauthEntry(provider, id, access, refresh string) PooledCredential {
	return PooledCredential{Provider: provider, ID: id, Label: "cred", AuthType: AuthTypeOAuth, Priority: 0, Source: "device_code", AccessToken: access, RefreshToken: refresh, Extra: map[string]any{}}
}

func TestGlobalWriteThroughPreservesConcurrentRootUpdate(t *testing.T) {
	profile, root := profileAndRoot(t)
	writeAuthStore(t, root, map[string]any{"version": 1,
		"providers":       map[string]any{"xai-oauth": map[string]any{"tokens": map[string]any{"access_token": "old-xai", "refresh_token": "old-r"}}},
		"credential_pool": map[string]any{"anthropic": []any{map[string]any{"id": "anthropic-existing"}}, "openrouter": []any{map[string]any{"id": "openrouter-existing"}}}})
	env := testEnv(t, profile)
	if env.Store.globalPath != filepath.Join(root, "auth.json") {
		t.Fatalf("the profile store must fall back to the root: %q", env.Store.globalPath)
	}
	rootStore := NewStore(root)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		writeThroughProviderStateToGlobalRoot(env, "xai-oauth", map[string]any{"tokens": map[string]any{"access_token": "new-xai", "refresh_token": "new-r"}})
	}()
	go func() {
		defer wg.Done()
		unlock, err := rootStore.Lock(AuthLockTimeout)
		if err != nil {
			t.Error(err)
			return
		}
		data, _ := rootStore.Load()
		rootStore.StoreProviderState(data, "openai-codex", map[string]any{"tokens": map[string]any{"access_token": "codex-a", "refresh_token": "codex-r"}}, false)
		pool, _ := data["credential_pool"].(map[string]any)
		if pool == nil {
			pool = map[string]any{}
			data["credential_pool"] = pool
		}
		pool["openai-codex"] = []any{map[string]any{"id": "codex-login"}}
		_ = rootStore.Save(data)
		unlock()
	}()
	wg.Wait()
	rootData := readAuthStore(t, root)
	providers := rootData["providers"].(map[string]any)
	if providers["xai-oauth"].(map[string]any)["tokens"].(map[string]any)["refresh_token"] != "new-r" {
		t.Fatal("the write through must land")
	}
	if providers["openai-codex"].(map[string]any)["tokens"].(map[string]any)["refresh_token"] != "codex-r" {
		t.Fatal("the concurrent login must survive")
	}
	pools := rootData["credential_pool"].(map[string]any)
	for _, key := range []string{"openai-codex", "anthropic", "openrouter"} {
		if len(pools[key].([]any)) != 1 {
			t.Fatalf("pool %s must survive: %v", key, pools[key])
		}
	}
}

func TestCodexPoolRefreshHoldsAuthStoreLockAcrossPost(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	held := false
	prev := tokensSingletonProviders["openai-codex"]
	spec := prev
	spec.refresh = func(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
		lock := env.Store.processLock(env.Store.Path())
		lock.state.Lock()
		held = lock.depth > 0 && lock.owner == goroutineID()
		lock.state.Unlock()
		return map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh", "last_refresh": "2020-01-02T00:00:00Z"}, nil
	}
	tokensSingletonProviders["openai-codex"] = spec
	t.Cleanup(func() { tokensSingletonProviders["openai-codex"] = prev })
	entry := oauthEntry("openai-codex", "codex-1", "stale-access", "stale-refresh")
	pool := NewPool(env, "openai-codex", []PooledCredential{entry})
	refreshed, ok := pool.refreshEntry(entry, true, "", false)
	if !ok || refreshed.AccessToken != "rotated-access" || refreshed.RefreshToken != "rotated-refresh" {
		t.Fatalf("rotation expected: %+v", refreshed)
	}
	if !held {
		t.Fatal("the single use token POST must run inside the auth store lock")
	}
}

func TestWriteThroughFiresOnEveryRefreshNotJustFirst(t *testing.T) {
	profile, root := profileAndRoot(t)
	writeAuthStore(t, root, map[string]any{"version": 1, "providers": map[string]any{"openai-codex": map[string]any{"tokens": map[string]any{"access_token": "root-ac", "refresh_token": "root-rf"}}}})
	writeAuthStore(t, profile, map[string]any{"version": 1})
	env := testEnv(t, profile)
	for i, pair := range [][2]string{{"ac1", "rf1"}, {"ac2", "rf2"}} {
		entry := oauthEntry("openai-codex", "c"+string(rune('1'+i)), pair[0], pair[1])
		pool := NewPool(env, "openai-codex", []PooledCredential{entry})
		pool.syncDeviceCodeEntryToAuthStore(entry)
		tokens := readAuthStore(t, root)["providers"].(map[string]any)["openai-codex"].(map[string]any)["tokens"].(map[string]any)
		if tokens["access_token"] != pair[0] || tokens["refresh_token"] != pair[1] {
			t.Fatalf("refresh %d must reach root: %v", i+1, tokens)
		}
		if providers, ok := readAuthStore(t, profile)["providers"].(map[string]any); ok {
			if _, shadow := providers["openai-codex"]; shadow {
				t.Fatal("the profile must not accrue a shadowing providers block")
			}
		}
	}
}

func anthropicTokenServer(t *testing.T, env *PoolEnv, handler func(refreshToken string) (map[string]any, int)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		body := map[string]any{}
		raw := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(raw)
		if json.Unmarshal(raw, &payload) == nil {
			body = payload
		} else {
			values := strings.Split(string(raw), "&")
			for _, kv := range values {
				k, v, _ := strings.Cut(kv, "=")
				body[k] = v
			}
		}
		resp, status := handler(anyString(body["refresh_token"]))
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(server.Close)
	env.HTTP = &http.Client{Transport: rewriteTransport{target: server.URL}}
	return server
}

// Sends every request to the test server regardless of the configured endpoint host
type rewriteTransport struct{ target string }

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	target := strings.TrimPrefix(rt.target, "http://")
	clone.URL.Scheme, clone.URL.Host = "http", target
	clone.Host = target
	return http.DefaultTransport.RoundTrip(clone)
}

func TestHermesPKCERefreshWritesBackToSingleton(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "anthropic", "providers": map[string]any{}})
	env := testEnv(t, h)
	if err := WriteHermesOAuthCredentials(env, "sk-ant-oat-rt0", "rt0", 0, ""); err != nil {
		t.Fatal(err)
	}
	anthropicTokenServer(t, &env, func(refreshToken string) (map[string]any, int) {
		return map[string]any{"access_token": "sk-ant-oat-rt1", "refresh_token": "rt1", "expires_in": 3600}, 200
	})
	entry := PooledCredential{Provider: "anthropic", ID: "pool-entry", Label: "cred", AuthType: AuthTypeOAuth, Source: "hermes_pkce", AccessToken: "sk-ant-oat-rt0", RefreshToken: "rt0", Extra: map[string]any{}}
	pool := NewPool(env, "anthropic", []PooledCredential{entry})
	updated, ok := pool.refreshEntry(entry, true, "", false)
	if !ok || updated.RefreshToken != "rt1" {
		t.Fatalf("refresh expected: %+v", updated)
	}
	if ReadHermesOAuthCredentials(env)["refreshToken"] != "rt1" {
		t.Fatal("the singleton must carry the rotated pair")
	}
	var reloaded []PooledCredential
	for _, e := range LoadPool(env, "anthropic").Entries() {
		if strings.HasSuffix(e.Source, "hermes_pkce") {
			reloaded = append(reloaded, e)
		}
	}
	if len(reloaded) == 0 || reloaded[0].RefreshToken != "rt1" {
		t.Fatalf("a fresh load must keep the rotation: %+v", reloaded)
	}
}

func TestManualHermesPKCERefreshDoesNotCreateDuplicateSingleton(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "anthropic", "providers": map[string]any{}})
	env := testEnv(t, h)
	anthropicTokenServer(t, &env, func(string) (map[string]any, int) {
		return map[string]any{"access_token": "manual-at-1", "refresh_token": "manual-rt-1", "expires_in": 3600}, 200
	})
	entry := PooledCredential{Provider: "anthropic", ID: "manual-entry", Label: "cred", AuthType: AuthTypeOAuth, Source: "manual:hermes_pkce", AccessToken: "manual-at-0", RefreshToken: "manual-rt-0", Extra: map[string]any{}}
	pool := NewPool(env, "anthropic", []PooledCredential{entry})
	refreshed, ok := pool.refreshEntry(entry, true, "", false)
	if !ok || refreshed.RefreshToken != "manual-rt-1" {
		t.Fatalf("refresh expected: %+v", refreshed)
	}
	if fileExists(HermesOAuthFile(env)) {
		t.Fatal("a pool owned manual row must not create a singleton")
	}
	var matching []PooledCredential
	for _, e := range LoadPool(env, "anthropic").Entries() {
		if e.ID == "manual-entry" {
			matching = append(matching, e)
		}
	}
	if len(matching) != 1 || matching[0].Source != "manual:hermes_pkce" || matching[0].RefreshToken != "manual-rt-1" {
		t.Fatalf("the manual row must persist its rotation: %+v", matching)
	}
}

// ---- test_anthropic_credential_persist_failure.py ----

const (
	staleAccess    = "sk-ant-oat01-stale"
	staleRefresh   = "sk-ant-ort01-stale"
	rotatedAccess  = "sk-ant-oat01-rotated"
	rotatedRefresh = "sk-ant-ort01-rotated"
)

func persistFailureHome(t *testing.T) (string, PoolEnv) {
	t.Helper()
	ResetSpentRotationsForTests()
	t.Cleanup(ResetSpentRotationsForTests)
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "anthropic", "providers": map[string]any{}})
	env := testEnv(t, h)
	anthropicTokenServer(t, &env, func(string) (map[string]any, int) {
		return map[string]any{"access_token": rotatedAccess, "refresh_token": rotatedRefresh, "expires_in": 3600}, 200
	})
	return h, env
}

// Fails the durable write of the singleton so the commit never lands, every other write keeps working
func breakDurableWrite(t *testing.T, path string) {
	t.Helper()
	prev := atomicWritePrivate
	atomicWritePrivate = func(target string, data []byte, mode os.FileMode) error {
		if filepath.Base(target) == filepath.Base(path) {
			return errors.New("permission denied")
		}
		return prev(target, data, mode)
	}
	t.Cleanup(func() { atomicWritePrivate = prev })
}

func restoreDurableWrite(t *testing.T) {
	t.Helper()
	atomicWritePrivate = config.AtomicWrite
}

func readClaudePair(t *testing.T) (string, string) {
	t.Helper()
	raw, err := os.ReadFile(ClaudeCodeCredentialsPath())
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	oauth := data["claudeAiOauth"].(map[string]any)
	return anyString(oauth["accessToken"]), anyString(oauth["refreshToken"])
}

func staleAnthropicEntry(source string) PooledCredential {
	return PooledCredential{Provider: "anthropic", ID: "anthropic-1", Label: "anthropic oauth", AuthType: AuthTypeOAuth, Source: source, AccessToken: staleAccess, RefreshToken: staleRefresh, ExpiresAtMS: 1000, Extra: map[string]any{}}
}

func TestClaudeCodeWriterRaisesInsteadOfSwallowing(t *testing.T) {
	_, env := persistFailureHome(t)
	writeClaudeCodeCredentials(t, staleAccess, staleRefresh, 1000)
	breakDurableWrite(t, ClaudeCodeCredentialsPath())
	err := WriteClaudeCodeCredentials(env, rotatedAccess, rotatedRefresh, 1000+3_600_000, nil, "")
	var persistErr *CredentialPersistError
	if !errors.As(err, &persistErr) {
		t.Fatalf("CredentialPersistError expected, got %v", err)
	}
	if a, r := readClaudePair(t); a != staleAccess || r != staleRefresh {
		t.Fatal("the failed commit must leave the file intact")
	}
}

func TestHermesOAuthWriterRaisesInsteadOfSwallowing(t *testing.T) {
	_, env := persistFailureHome(t)
	if err := WriteHermesOAuthCredentials(env, staleAccess, staleRefresh, 1000, ""); err != nil {
		t.Fatal(err)
	}
	breakDurableWrite(t, HermesOAuthFile(env))
	err := WriteHermesOAuthCredentials(env, rotatedAccess, rotatedRefresh, 1000+3_600_000, "")
	var persistErr *CredentialPersistError
	if !errors.As(err, &persistErr) {
		t.Fatalf("CredentialPersistError expected, got %v", err)
	}
	if ReadHermesOAuthCredentials(env)["refreshToken"] != staleRefresh {
		t.Fatal("stale pair must survive")
	}
}

func TestFailedWriteLeavesNoTempFileBehind(t *testing.T) {
	_, env := persistFailureHome(t)
	writeClaudeCodeCredentials(t, staleAccess, staleRefresh, 1000)
	breakDurableWrite(t, ClaudeCodeCredentialsPath())
	if err := WriteClaudeCodeCredentials(env, rotatedAccess, rotatedRefresh, 0, nil, ""); err == nil {
		t.Fatal("error expected")
	}
	entries, _ := os.ReadDir(filepath.Dir(ClaudeCodeCredentialsPath()))
	for _, e := range entries {
		if e.Name() != ".credentials.json" {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestDirectResolverFailsClosedWhenRotationCannotCommit(t *testing.T) {
	_, env := persistFailureHome(t)
	writeClaudeCodeCredentials(t, staleAccess, staleRefresh, 1000)
	breakDurableWrite(t, ClaudeCodeCredentialsPath())
	creds := ReadClaudeCodeCredentials(env)
	if creds == nil {
		t.Fatal("credentials expected")
	}
	if refreshClaudeCodeOAuthToken(env, creds) != "" {
		t.Fatal("a refresh whose write failed must not hand back a token")
	}
	if a, r := readClaudePair(t); a != staleAccess || r != staleRefresh {
		t.Fatal("file must stay intact")
	}
}

func TestPoolClaudeCodeFailsClosedAndReloadCannotResurrect(t *testing.T) {
	_, env := persistFailureHome(t)
	writeClaudeCodeCredentials(t, staleAccess, staleRefresh, 1000)
	breakDurableWrite(t, ClaudeCodeCredentialsPath())
	entry := staleAnthropicEntry("claude_code")
	pool := NewPool(env, "anthropic", []PooledCredential{entry})
	if _, ok := pool.refreshEntry(entry, true, "", false); ok {
		t.Fatal("an uncommitted rotation must not be returned")
	}
	quarantined := pool.Entries()[0]
	if quarantined.LastStatus != StatusDead || quarantined.LastErrorReason != CredentialPersistFailedReason || quarantined.AccessToken != staleAccess || quarantined.RefreshToken != staleRefresh {
		t.Fatalf("quarantine expected: %+v", quarantined)
	}
	var reloaded []PooledCredential
	for _, e := range LoadPool(env, "anthropic").Entries() {
		if e.Source == "claude_code" {
			reloaded = append(reloaded, e)
		}
	}
	if len(reloaded) == 0 || reloaded[0].RefreshToken != staleRefresh || reloaded[0].LastStatus != StatusDead {
		t.Fatalf("a reload must not resurrect the spent pair: %+v", reloaded)
	}
}

func TestReauthenticationClearsThePersistFailureQuarantine(t *testing.T) {
	_, env := persistFailureHome(t)
	writeClaudeCodeCredentials(t, staleAccess, staleRefresh, 1000)
	breakDurableWrite(t, ClaudeCodeCredentialsPath())
	entry := staleAnthropicEntry("claude_code")
	pool := NewPool(env, "anthropic", []PooledCredential{entry})
	if _, ok := pool.refreshEntry(entry, true, "", false); ok || pool.Entries()[0].LastStatus != StatusDead {
		t.Fatal("quarantine expected")
	}
	restoreDurableWrite(t)
	writeClaudeCodeCredentials(t, "sk-ant-oat01-relogin", "sk-ant-ort01-relogin", time.Now().UnixMilli()+3_600_000)
	var reloaded []PooledCredential
	for _, e := range LoadPool(env, "anthropic").Entries() {
		if e.Source == "claude_code" {
			reloaded = append(reloaded, e)
		}
	}
	if len(reloaded) == 0 || reloaded[0].RefreshToken != "sk-ant-ort01-relogin" || reloaded[0].LastStatus == StatusDead {
		t.Fatalf("a re-login must clear the quarantine: %+v", reloaded)
	}
}

func TestPoolHermesPKCEFailsClosedAndReloadCannotResurrect(t *testing.T) {
	_, env := persistFailureHome(t)
	if err := WriteHermesOAuthCredentials(env, staleAccess, staleRefresh, 1000, ""); err != nil {
		t.Fatal(err)
	}
	breakDurableWrite(t, HermesOAuthFile(env))
	entry := staleAnthropicEntry("hermes_pkce")
	pool := NewPool(env, "anthropic", []PooledCredential{entry})
	if _, ok := pool.refreshEntry(entry, true, "", false); ok {
		t.Fatal("uncommitted rotation must fail closed")
	}
	quarantined := pool.Entries()[0]
	if quarantined.LastStatus != StatusDead || quarantined.LastErrorReason != CredentialPersistFailedReason || quarantined.RefreshToken != staleRefresh {
		t.Fatalf("quarantine expected: %+v", quarantined)
	}
	if ReadHermesOAuthCredentials(env)["refreshToken"] != staleRefresh {
		t.Fatal("singleton must stay stale")
	}
	var reloaded []PooledCredential
	for _, e := range LoadPool(env, "anthropic").Entries() {
		if e.Source == "hermes_pkce" {
			reloaded = append(reloaded, e)
		}
	}
	if len(reloaded) == 0 || reloaded[0].RefreshToken != staleRefresh || reloaded[0].LastStatus != StatusDead {
		t.Fatalf("reload must keep the quarantine: %+v", reloaded)
	}
}

func TestRetryPathFailsClosedWhenRotationCannotCommit(t *testing.T) {
	_, env := persistFailureHome(t)
	var posts []string
	anthropicTokenServer(t, &env, func(refreshToken string) (map[string]any, int) {
		posts = append(posts, refreshToken)
		if refreshToken == staleRefresh {
			return map[string]any{"error": "invalid_grant"}, 400
		}
		return map[string]any{"access_token": rotatedAccess, "refresh_token": rotatedRefresh, "expires_in": 3600}, 200
	})
	writeClaudeCodeCredentials(t, "sk-ant-oat01-winner", "sk-ant-ort01-winner", 1000)
	breakDurableWrite(t, ClaudeCodeCredentialsPath())
	entry := staleAnthropicEntry("claude_code")
	pool := NewPool(env, "anthropic", []PooledCredential{entry})
	if _, ok := pool.refreshEntryImpl(entry, true, "", false); ok {
		t.Fatal("the retry path must fail closed")
	}
	if len(posts) != 2 || posts[0] != staleRefresh || posts[1] != "sk-ant-ort01-winner" {
		t.Fatalf("the retry branch must re-POST the synced token exactly once: %v", posts)
	}
	quarantined := pool.Entries()[0]
	if quarantined.LastStatus != StatusDead || quarantined.LastErrorReason != CredentialPersistFailedReason || quarantined.AccessToken == rotatedAccess {
		t.Fatalf("quarantine expected: %+v", quarantined)
	}
	if a, r := readClaudePair(t); a != "sk-ant-oat01-winner" || r != "sk-ant-ort01-winner" {
		t.Fatal("the winner's pair must stay on disk")
	}
}

// ---- test_credential_pool_anthropic_refresh_race.py ----

type singleUseTokenServer struct {
	mu       sync.Mutex
	spent    map[string]bool
	rotation int
	calls    []string
}

func (s *singleUseTokenServer) refresh(refreshToken string) (map[string]any, int) {
	s.mu.Lock()
	s.calls = append(s.calls, refreshToken)
	s.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spent[refreshToken] {
		return map[string]any{"error": "invalid_grant", "error_description": "refresh token already used"}, 400
	}
	s.spent[refreshToken] = true
	s.rotation++
	n := string(rune('0' + s.rotation))
	return map[string]any{"access_token": "sk-ant-oat-rotated-" + n, "refresh_token": "sk-ant-ort-rotated-" + n, "expires_in": 3600}, 200
}

func raceEntry(source string) PooledCredential {
	return PooledCredential{Provider: "anthropic", ID: "pool-entry", Label: "anthropic oauth", AuthType: AuthTypeOAuth, Source: source, AccessToken: "stale-at", RefreshToken: "stale-rt", Extra: map[string]any{}}
}

func TestConcurrentHermesPKCERefreshRecoversViaPoolStore(t *testing.T) {
	ResetSpentRotationsForTests()
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "anthropic", "providers": map[string]any{}})
	env := testEnv(t, h)
	server := &singleUseTokenServer{spent: map[string]bool{}}
	anthropicTokenServer(t, &env, server.refresh)
	entry := raceEntry("manual:hermes_pkce")
	poolA := NewPool(env, "anthropic", []PooledCredential{entry})
	poolB := NewPool(env, "anthropic", []PooledCredential{entry})
	poolA.persist(false, nil, nil)
	results := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for key, pool := range map[string]*Pool{"a": poolA, "b": poolB} {
		wg.Add(1)
		go func(key string, pool *Pool) {
			defer wg.Done()
			_, ok := pool.refreshEntry(pool.Entries()[0], true, "", false)
			mu.Lock()
			results[key] = ok
			mu.Unlock()
		}(key, pool)
	}
	wg.Wait()
	if len(server.calls) < 1 || len(server.calls) > 2 {
		t.Fatalf("unexpected call count: %v", server.calls)
	}
	for _, c := range server.calls {
		if c != "stale-rt" {
			t.Fatalf("only the on disk token may be spent: %v", server.calls)
		}
	}
	if !results["a"] || !results["b"] {
		t.Fatalf("both processes must end with a usable credential: %v", results)
	}
	for _, pool := range []*Pool{poolA, poolB} {
		e := pool.Entries()[0]
		if e.LastStatus == StatusExhausted || !strings.HasPrefix(e.AccessToken, "sk-ant-oat-rotated-") {
			t.Fatalf("the loser must adopt the winner's rotation: %+v", e)
		}
	}
}

func TestConcurrentClaudeCodeRefreshRecoversViaCredentialsFile(t *testing.T) {
	ResetSpentRotationsForTests()
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "anthropic", "providers": map[string]any{}})
	env := testEnv(t, h)
	server := &singleUseTokenServer{spent: map[string]bool{}}
	anthropicTokenServer(t, &env, server.refresh)
	writeClaudeCodeCredentials(t, "stale-at", "stale-rt", 1000)
	entry := raceEntry("claude_code")
	poolA := NewPool(env, "anthropic", []PooledCredential{entry})
	poolB := NewPool(env, "anthropic", []PooledCredential{entry})
	results := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for key, pool := range map[string]*Pool{"a": poolA, "b": poolB} {
		wg.Add(1)
		go func(key string, pool *Pool) {
			defer wg.Done()
			_, ok := pool.refreshEntry(pool.Entries()[0], true, "", false)
			mu.Lock()
			results[key] = ok
			mu.Unlock()
		}(key, pool)
	}
	wg.Wait()
	if !results["a"] || !results["b"] {
		t.Fatalf("both claude_code processes must recover: %v", results)
	}
}

// ---- test_credential_pool_terminal_refresh_visibility.py ----

func barePool(t *testing.T, provider string, sink *recordingHandler) *Pool {
	t.Helper()
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	env := testEnv(t, h)
	env.Log = newRecordingLogger(sink)
	return NewPool(env, provider, nil)
}

func deadEntry(provider, source string) PooledCredential {
	return PooledCredential{ID: "e1", Provider: provider, AuthType: "oauth", AccessToken: "dead-access", RefreshToken: "dead-refresh", Label: "e1", Source: source, Priority: 0, Extra: map[string]any{}}
}

func TestTerminalRefreshQuarantineWarnsWithReauthHint(t *testing.T) {
	for _, provider := range []string{"openai-codex", "nous"} {
		t.Run(provider, func(t *testing.T) {
			sink := &recordingHandler{}
			pool := barePool(t, provider, sink)
			entry := deadEntry(provider, "device_code")
			pool.mu.Lock()
			pool.entries = []PooledCredential{entry}
			pool.mu.Unlock()
			cause := &AuthError{Message: "invalid_grant", Provider: provider, Code: "invalid_grant", ReloginRequired: true}
			if _, ok := pool.recoverFailedRefresh(entry, cause, false); ok {
				t.Fatal("a terminal refresh recovers nothing")
			}
			warnings := sink.containing("terminally invalid")
			if len(warnings) != 1 || !strings.Contains(warnings[0], "icebath auth add "+provider) || !strings.Contains(warnings[0], "invalid_grant") {
				t.Fatalf("one WARNING with the re-auth hint expected: %v", warnings)
			}
			if len(pool.Entries()) != 0 {
				t.Fatal("the singleton seeded row must be quarantined")
			}
		})
	}
}

func TestAnthropicDeadGrantWarnsAndMarksDead(t *testing.T) {
	sink := &recordingHandler{}
	pool := barePool(t, "anthropic", sink)
	entry := deadEntry("anthropic", "manual:hermes_pkce")
	pool.mu.Lock()
	pool.entries = []PooledCredential{entry}
	pool.mu.Unlock()
	cause := &AnthropicOAuthError{Status: 400, Code: "invalid_grant", Description: "refresh token revoked", What: "refresh"}
	if _, ok := pool.recoverFailedRefresh(entry, cause, false); ok {
		t.Fatal("a dead grant recovers nothing")
	}
	rowEntry := pool.Entries()[0]
	if rowEntry.LastStatus != StatusDead || rowEntry.LastErrorReason != "invalid_grant" {
		t.Fatalf("DEAD with the grant code expected: %+v", rowEntry)
	}
	warnings := sink.containing("terminally invalid")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "icebath auth add anthropic") {
		t.Fatalf("WARNING with the hint expected: %v", warnings)
	}
}

func TestAnthropicTransientRefreshFailureStaysExhausted(t *testing.T) {
	sink := &recordingHandler{}
	pool := barePool(t, "anthropic", sink)
	entry := deadEntry("anthropic", "manual:hermes_pkce")
	pool.mu.Lock()
	pool.entries = []PooledCredential{entry}
	pool.mu.Unlock()
	if _, ok := pool.recoverFailedRefresh(entry, errors.New("token endpoint timed out"), false); ok {
		t.Fatal("transient failure recovers nothing")
	}
	if pool.Entries()[0].LastStatus != StatusExhausted {
		t.Fatal("a transient failure is benched, not killed")
	}
	if len(sink.containing("terminally invalid")) != 0 {
		t.Fatal("no WARNING for a transient failure")
	}
}

func TestSurvivingManualEntryIsMarkedDeadAfterTerminalRefresh(t *testing.T) {
	sink := &recordingHandler{}
	pool := barePool(t, "openai-codex", sink)
	entry := deadEntry("openai-codex", "manual:device_code")
	pool.mu.Lock()
	pool.entries = []PooledCredential{entry}
	pool.mu.Unlock()
	cause := &AuthError{Message: "invalid_grant", Provider: "openai-codex", Code: "invalid_grant", ReloginRequired: true}
	if _, ok := pool.recoverFailedRefresh(entry, cause, false); ok {
		t.Fatal("no recovery")
	}
	entries := pool.Entries()
	if len(entries) != 1 || entries[0].ID != "e1" || entries[0].LastStatus != StatusDead {
		t.Fatalf("the manual row survives the quarantine as DEAD: %+v", entries)
	}
}

func TestNousTransientErrorStillBenched(t *testing.T) {
	sink := &recordingHandler{}
	pool := barePool(t, "nous", sink)
	entry := deadEntry("nous", "device_code")
	pool.mu.Lock()
	pool.entries = []PooledCredential{entry}
	pool.mu.Unlock()
	if _, ok := pool.recoverFailedRefresh(entry, errors.New("boom"), false); ok {
		t.Fatal("no recovery")
	}
	if pool.Entries()[0].LastStatus != StatusExhausted {
		t.Fatal("benched expected")
	}
	if len(sink.containing("terminally invalid")) != 0 {
		t.Fatal("no terminal WARNING")
	}
}

// ---- test_credential_pool_nous_refresh_stampede.py and credential_auth_recovery ----

func nousInvokeJWT(seconds float64) string {
	return jwtWithClaims(map[string]any{"sub": "u", "scope": "inference:invoke", "exp": int(nowEpoch() + seconds)})
}

func seedNous(t *testing.T, h, token, refreshToken string) {
	t.Helper()
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "nous", "providers": map[string]any{"nous": map[string]any{
		"client_id": "icebath-cli", "portal_base_url": DefaultNousPortalURL, "inference_base_url": DefaultNousInferenceURL, "token_type": "Bearer",
		"scope": DefaultNousScope, "access_token": token, "refresh_token": refreshToken, "expires_at": isoIn(3000), "expires_in": 3000}}})
}

// Stands in for the Portal, rotating the stored pair under the auth store the way the real resolver does
func fakeNousResolver(t *testing.T, posts *[]string) {
	t.Helper()
	prev := nousRuntimeCredentialsFunc
	nousRuntimeCredentialsFunc = func(env PoolEnv, force bool, stale string) (map[string]any, error) {
		return nil, env.Store.Update(func(data AuthData) (bool, error) {
			state := env.Store.ProviderState(data, "nous")
			*posts = append(*posts, anyString(state["refresh_token"]))
			state["access_token"] = nousInvokeJWT(3600)
			state["refresh_token"] = "rt-minted-" + string(rune('0'+len(*posts)))
			env.Store.StoreProviderState(data, "nous", state, false)
			return true, nil
		})
	}
	t.Cleanup(func() { nousRuntimeCredentialsFunc = prev })
}

func TestForcedRefreshNeverAdoptsTheBearerThatJustFailed(t *testing.T) {
	h := newTestHome(t)
	seedNous(t, h, nousInvokeJWT(2000), "rt-old")
	env := testEnv(t, h)
	pool := LoadPool(env, "nous")
	entry := pool.Entries()[0]
	failing := nousInvokeJWT(3000)
	seedNous(t, h, failing, "rt-current")
	var posts []string
	fakeNousResolver(t, &posts)
	refreshed, ok := pool.TryRefreshMatching(failing, entry.ID)
	if !ok {
		t.Fatal("refresh expected")
	}
	if refreshed.RuntimeAPIKey() == failing {
		t.Fatal("the refresh handed back the bearer that just failed")
	}
	if len(posts) != 1 || posts[0] != "rt-current" {
		t.Fatalf("exactly one refresh POST expected, got %v", posts)
	}
}

type fakeAuthAgent struct {
	apiKey   string
	swapped  []string
	episodes map[string]*AuthEpisode
}

func (a *fakeAuthAgent) Provider() string                      { return "nous" }
func (a *fakeAuthAgent) APIMode() APIMode                      { return ModeChatCompletions }
func (a *fakeAuthAgent) Interrupted() bool                     { return false }
func (a *fakeAuthAgent) TouchActivity(string)                  {}
func (a *fakeAuthAgent) Print(string)                          {}
func (a *fakeAuthAgent) AuthEpisodes() map[string]*AuthEpisode { return a.episodes }
func (a *fakeAuthAgent) SwapCredential(entry PooledCredential) bool {
	a.apiKey = entry.RuntimeAPIKey()
	a.swapped = append(a.swapped, a.apiKey)
	return true
}

func TestOneRefreshPerEpisodeThenBoundedBackoff(t *testing.T) {
	h := newTestHome(t)
	key := nousInvokeJWT(3000)
	seedNous(t, h, key, "rt-seed")
	env := testEnv(t, h)
	pool := LoadPool(env, "nous")
	entry := pool.Entries()[0]
	var posts []string
	fakeNousResolver(t, &posts)
	prevBackoff := TransientAuthBackoff
	TransientAuthBackoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}
	t.Cleanup(func() { TransientAuthBackoff = prevBackoff })
	agent := &fakeAuthAgent{apiKey: key, episodes: map[string]*AuthEpisode{}}
	var outcomes []bool
	for i := 0; i < 5; i++ {
		recovered, _ := RecoverAuthFailure(agent, pool, AuthRecoveryRequest{StatusCode: 401, ErrorContext: map[string]any{"message": "authentication required"},
			APIKeyHint: agent.apiKey, CredentialID: entry.ID, RotateAndSwap: func(int, string) bool { return false }})
		outcomes = append(outcomes, recovered)
	}
	want := []bool{true, true, true, true, false}
	for i := range want {
		if outcomes[i] != want[i] {
			t.Fatalf("outcomes %v, want %v", outcomes, want)
		}
	}
	if len(posts) != 1 {
		t.Fatalf("one 401 episode redeemed the refresh token %d times", len(posts))
	}
	if len(agent.swapped) == 0 {
		t.Fatal("the fresh key must be swapped in")
	}
	for _, k := range agent.swapped[1:] {
		if k != agent.swapped[0] {
			t.Fatal("backoff retries must keep the freshly minted key")
		}
	}
}

// ---- test_credential_pool_deferred_refresh.py and refresh sweep spacing ----

func TestSelectDoesNotHoldPoolLockDuringDeferredRefresh(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	entry := PooledCredential{Provider: "openai-codex", ID: "codex-1", Label: "test codex", AuthType: AuthTypeOAuth, Source: "device_code",
		AccessToken: jwtWithClaims(map[string]any{"exp": 1}), RefreshToken: "rt-stale", Extra: map[string]any{}}
	pool := NewPool(env, "openai-codex", []PooledCredential{entry})
	lockFree := false
	prev := tokensSingletonProviders["openai-codex"]
	spec := prev
	spec.refresh = func(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
		if pool.mu.TryLock() {
			lockFree = true
			pool.mu.Unlock()
		}
		return map[string]any{"access_token": jwtWithClaims(map[string]any{"exp": nowEpoch() + 1e6}), "refresh_token": "rt-fresh"}, nil
	}
	tokensSingletonProviders["openai-codex"] = spec
	t.Cleanup(func() { tokensSingletonProviders["openai-codex"] = prev })
	selected, ok := pool.Select("")
	if !lockFree {
		t.Fatal("select held the pool lock during the deferred refresh network window")
	}
	if !ok || selected.RefreshToken != "rt-fresh" {
		t.Fatalf("the refreshed entry must be selected: %+v", selected)
	}
}

func TestPendingRefreshSweepLeavesWaiterWindowsBetweenHolds(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	var slept []time.Duration
	prevSleep := sleepFunc
	sleepFunc = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { sleepFunc = prevSleep })
	var refreshed int
	prev := tokensSingletonProviders["openai-codex"]
	spec := prev
	spec.refresh = func(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
		refreshed++
		return map[string]any{"access_token": jwtWithClaims(map[string]any{"exp": nowEpoch() + 1e6}), "refresh_token": refreshToken + "-new"}, nil
	}
	tokensSingletonProviders["openai-codex"] = spec
	t.Cleanup(func() { tokensSingletonProviders["openai-codex"] = prev })
	var entries []PooledCredential
	for i := 0; i < 3; i++ {
		entries = append(entries, PooledCredential{Provider: "openai-codex", ID: "c" + string(rune('1'+i)), Label: "c", AuthType: AuthTypeOAuth, Source: "device_code",
			AccessToken: jwtWithClaims(map[string]any{"exp": 1}), RefreshToken: "rt-" + string(rune('1'+i)), Extra: map[string]any{}})
	}
	pool := NewPool(env, "openai-codex", entries)
	pool.refreshPendingEntries(entries)
	if refreshed != 3 {
		t.Fatalf("every entry refreshed, got %d", refreshed)
	}
	if len(slept) != 2 || slept[0] < 400*time.Millisecond || slept[1] < 400*time.Millisecond {
		t.Fatalf("a waiter window must separate consecutive holds: %v", slept)
	}
}

// ---- acquire lease after a deferred refresh ----

func TestAcquireLeaseReselectsAfterDeferredRefresh(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	entry := PooledCredential{Provider: "openai-codex", ID: "e1", Label: "e1", AuthType: AuthTypeOAuth, Source: "device_code",
		AccessToken: jwtWithClaims(map[string]any{"exp": 1}), RefreshToken: "rt", Extra: map[string]any{}}
	pool := NewPool(env, "openai-codex", []PooledCredential{entry})
	calls := 0
	prev := tokensSingletonProviders["openai-codex"]
	spec := prev
	spec.refresh = func(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
		calls++
		return map[string]any{"access_token": jwtWithClaims(map[string]any{"exp": nowEpoch() + 1e6}), "refresh_token": "rt-new"}, nil
	}
	tokensSingletonProviders["openai-codex"] = spec
	t.Cleanup(func() { tokensSingletonProviders["openai-codex"] = prev })
	lease, ok := pool.AcquireLease("")
	if calls != 1 || !ok || lease != "e1" {
		t.Fatalf("lease must be granted after the deferred refresh: calls=%d ok=%t lease=%q", calls, ok, lease)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.activeLeases["e1"] != 1 {
		t.Fatal("the lease must be recorded")
	}
}

func TestAcquireLeaseStillNoneWhenRefreshDoesNotHelp(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	entry := PooledCredential{Provider: "openai-codex", ID: "e1", Label: "e1", AuthType: AuthTypeOAuth, Source: "device_code",
		AccessToken: jwtWithClaims(map[string]any{"exp": 1}), RefreshToken: "rt", Extra: map[string]any{}}
	pool := NewPool(env, "openai-codex", []PooledCredential{entry})
	calls := 0
	prev := tokensSingletonProviders["openai-codex"]
	spec := prev
	spec.refresh = func(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
		calls++
		return nil, errors.New("token endpoint down")
	}
	tokensSingletonProviders["openai-codex"] = spec
	t.Cleanup(func() { tokensSingletonProviders["openai-codex"] = prev })
	if _, ok := pool.AcquireLease(""); ok {
		t.Fatal("no lease when the refresh leaves nothing available")
	}
	if calls != 1 {
		t.Fatalf("the retry must not refresh repeatedly, got %d", calls)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.activeLeases) != 0 {
		t.Fatal("no lease recorded")
	}
}
