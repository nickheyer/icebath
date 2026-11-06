package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Swaps the provider client resolver for a scripted one counting its calls
func auxStubResolveProviderClient(t *testing.T, build func(provider, model string, opts ProviderClientOptions) (AuxClient, string)) *int {
	t.Helper()
	calls := 0
	prev := auxResolveProviderClientFn
	auxResolveProviderClientFn = func(_ context.Context, provider, model string, opts ProviderClientOptions) (AuxClient, string, error) {
		calls++
		c, m := build(provider, model, opts)
		return c, m, nil
	}
	t.Cleanup(func() { auxResolveProviderClientFn = prev })
	return &calls
}

func auxCacheKeyRuntime() RuntimeMainOptions {
	return RuntimeMainOptions{BaseURL: "http://llama-swap.test/v1", APIKey: "local-key", APIMode: "chat_completions", AuthMode: "api_key"}
}

func TestImplicitRuntimeCacheKeyCoversFullConnectionAndAuthSurface(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	base := auxCacheKeyRuntime()
	baseline, _, err := auxClientCacheKeyFor(SetRuntimeMain(ctx, "custom:llama-swap", "same-model", base), "auto", false, "", nil, "", nil, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	variants := []struct {
		provider string
		opts     RuntimeMainOptions
	}{
		{"custom:other", base},
		{"custom:llama-swap", RuntimeMainOptions{BaseURL: "https://other.test/v1", APIKey: "local-key", APIMode: "chat_completions", AuthMode: "api_key"}},
		{"custom:llama-swap", RuntimeMainOptions{BaseURL: base.BaseURL, APIKey: "other-key", APIMode: "chat_completions", AuthMode: "api_key"}},
		{"custom:llama-swap", RuntimeMainOptions{BaseURL: base.BaseURL, APIKey: "local-key", APIMode: "codex_responses", AuthMode: "api_key"}},
		{"custom:llama-swap", RuntimeMainOptions{BaseURL: base.BaseURL, APIKey: func() string { return "token" }, APIMode: "chat_completions", AuthMode: "entra_id"}},
	}
	seen := map[auxClientCacheKey]bool{}
	for i, v := range variants {
		key, _, err := auxClientCacheKeyFor(SetRuntimeMain(ctx, v.provider, "same-model", v.opts), "auto", false, "", nil, "", nil, false, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if key == baseline || seen[key] {
			t.Fatalf("variant %d shares a key: %+v", i, key)
		}
		seen[key] = true
	}
	if got := auxNormalizeMainRuntime(ClearRuntimeMain(SetRuntimeMain(ctx, "custom:x", "turn-model", base)), nil); len(got) != 0 {
		t.Fatalf("cleared runtime %v", got)
	}
}

func TestCallableRuntimeAPIKeysAreSecretFreeDiscriminators(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	first := func() string { return "first-super-secret-token" }
	second := func() string { return "second-super-secret-token" }
	runtime := func(key any) map[string]any {
		return map[string]any{"provider": "custom:llama-swap", "model": "same", "base_url": "http://llama-swap.test/v1", "api_key": key, "api_mode": "chat_completions", "auth_mode": "api_key"}
	}
	a, keepA, _ := auxClientCacheKeyFor(ctx, "auto", false, "", nil, "", runtime(first), false, "", "")
	b, _, _ := auxClientCacheKeyFor(ctx, "auto", false, "", nil, "", runtime(second), false, "", "")
	if a == b || len(keepA) != 1 {
		t.Fatalf("callable keys not discriminated %+v %+v retained %d", a, b, len(keepA))
	}
	rendered := fmt.Sprintf("%+v %+v", a, b)
	if strings.Contains(rendered, "super-secret") {
		t.Fatalf("secret leaked %s", rendered)
	}
	s1, s2 := "first-literal-super-secret", "second-literal-super-secret"
	c, _, _ := auxClientCacheKeyFor(ctx, "auto", false, "", s1, "", runtime(s1), false, "", "")
	d, _, _ := auxClientCacheKeyFor(ctx, "auto", false, "", s2, "", runtime(s2), false, "", "")
	if c == d || strings.Contains(fmt.Sprintf("%+v %+v", c, d), "literal-super-secret") {
		t.Fatalf("string keys %+v %+v", c, d)
	}
}

func TestClientCacheKeyIsScopedPerProfileHomeAndModel(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	a, b := t.TempDir(), t.TempDir()
	ka, _, _ := auxClientCacheKeyFor(home.WithOverride(ctx, a), "nous", false, "https://inf.example", nil, "", nil, false, "", "m")
	kb, _, _ := auxClientCacheKeyFor(home.WithOverride(ctx, b), "nous", false, "https://inf.example", nil, "", nil, false, "", "m")
	if ka == kb {
		t.Fatal("profiles share a cache key")
	}
	km, _, _ := auxClientCacheKeyFor(home.WithOverride(ctx, a), "nous", false, "https://inf.example", nil, "", nil, false, "", "other")
	if km == ka {
		t.Fatal("model does not participate in the cache key")
	}
}

func TestAutoClientCacheKeyTracksFastModelPreference(t *testing.T) {
	plain := auxMainTestHome(t, "auxiliary:\n  title_generation:\n    provider: auto\n")
	k1, _, err := auxClientCacheKeyFor(plain, "auto", false, "", nil, "", nil, false, "title_generation", "")
	if err != nil {
		t.Fatal(err)
	}
	fast := auxMainTestHome(t, "auxiliary:\n  title_generation:\n    provider: auto\n    fast: true\n")
	k2, _, err := auxClientCacheKeyFor(fast, "auto", false, "", nil, "", nil, false, "title_generation", "")
	if err != nil {
		t.Fatal(err)
	}
	if k1.Task == k2.Task && k1.Task != "" {
		pref1, _ := auxTaskPrefersFastModel(plain, "title_generation")
		pref2, _ := auxTaskPrefersFastModel(fast, "title_generation")
		if pref1 != pref2 {
			t.Fatalf("fast preference not in key %q %q", k1.Task, k2.Task)
		}
	}
}

func TestCleanupAndShutdownCloseSemantics(t *testing.T) {
	auxIsolateClientCache(t)
	closedLoop, liveLoop := &auxTestLoop{closed: true}, &auxTestLoop{}
	awaitedClosed, awaitedLive := &auxAwaitableTestClient{}, &auxAwaitableTestClient{}
	auxClientCacheMu.Lock()
	auxClientCacheSetLocked(auxClientCacheKey{Provider: "test_async_close", Async: true}, auxCachedClient{Client: awaitedClosed, BoundLoop: closedLoop})
	auxClientCacheMu.Unlock()
	CleanupStaleAsyncClients()
	if !awaitedClosed.awaited || !awaitedClosed.neutered {
		t.Fatalf("closed loop client awaited %v neutered %v", awaitedClosed.awaited, awaitedClosed.neutered)
	}
	locked := &auxLockProbeClient{}
	auxClientCacheMu.Lock()
	auxClientCacheSetLocked(auxClientCacheKey{Provider: "test_shutdown_foreign_loop", Async: true}, auxCachedClient{Client: awaitedLive, BoundLoop: liveLoop})
	auxClientCacheSetLocked(auxClientCacheKey{Provider: "test_shutdown_lock"}, auxCachedClient{Client: locked})
	auxClientCacheMu.Unlock()
	ShutdownCachedClients(context.Background())
	if awaitedLive.awaited || !awaitedLive.neutered {
		t.Fatalf("live foreign loop client awaited %v neutered %v", awaitedLive.awaited, awaitedLive.neutered)
	}
	if len(locked.observed) != 1 || !locked.observed[0] {
		t.Fatalf("shutdown closed under the cache lock %v", locked.observed)
	}
	if len(auxClientCache) != 0 {
		t.Fatal("cache not cleared")
	}
	owned := &auxAwaitableTestClient{}
	auxClientCacheMu.Lock()
	auxClientCacheSetLocked(auxClientCacheKey{Provider: "current"}, auxCachedClient{Client: owned, BoundLoop: liveLoop})
	auxClientCacheMu.Unlock()
	ShutdownCachedClients(WithAuxiliaryEventLoop(context.Background(), liveLoop))
	if !owned.awaited {
		t.Fatal("current loop client not awaited")
	}
}

func TestSameKeyReplacesStaleLoopEntryAndLoopsIsolate(t *testing.T) {
	auxIsolateClientCache(t)
	ctx := auxMainTestHome(t, "")
	calls := auxStubResolveProviderClient(t, func(provider, model string, _ ProviderClientOptions) (AuxClient, string) {
		return &auxCloseCounter{}, "new-model"
	})
	oldLoop := &auxTestLoop{closed: true}
	key, _, _ := auxClientCacheKeyFor(ctx, "test_replace", true, "", nil, "", nil, false, "", "")
	old := &auxCloseCounter{}
	auxClientCacheMu.Lock()
	auxClientCacheSetLocked(key, auxCachedClient{Client: old, DefaultModel: "old-model", BoundLoop: oldLoop})
	auxClientCacheMu.Unlock()
	loopA, loopB := &auxTestLoop{}, &auxTestLoop{}
	c1, m1, err := auxGetCachedClient(WithAuxiliaryEventLoop(ctx, loopA), auxCachedClientRequest{Provider: "test_replace", AsyncMode: true})
	if err != nil || m1 != "new-model" || old.closes != 1 || len(auxClientCache) != 1 || auxClientCache[key].DefaultModel != "new-model" {
		t.Fatalf("stale entry not replaced model %q closes %d size %d err %v", m1, old.closes, len(auxClientCache), err)
	}
	same, _, _ := auxGetCachedClient(WithAuxiliaryEventLoop(ctx, loopA), auxCachedClientRequest{Provider: "test_replace", AsyncMode: true})
	other, _, _ := auxGetCachedClient(WithAuxiliaryEventLoop(ctx, loopB), auxCachedClientRequest{Provider: "test_replace", AsyncMode: true})
	if same != c1 || other == c1 || *calls != 2 || len(auxClientCache) != 1 {
		t.Fatalf("loop isolation same %v other %v calls %d size %d", same == c1, other == c1, *calls, len(auxClientCache))
	}
}

func TestProbeClientsAreNeverCachedAndSlashModelsKeepOpenRouterOnly(t *testing.T) {
	auxIsolateClientCache(t)
	ctx := auxMainTestHome(t, "")
	auxStubResolveProviderClient(t, func(provider, model string, _ ProviderClientOptions) (AuxClient, string) {
		return &auxProbeClient{baseURL: "https://api.gmi.ai/v1"}, "gmi-default"
	})
	if _, m, _ := auxGetCachedClient(AuxProbeMode(ctx), auxCachedClientRequest{Provider: "gmi", Model: "org/slash"}); m != "org/slash" || len(auxClientCache) != 0 {
		t.Fatalf("probe cached size %d model %q", len(auxClientCache), m)
	}
	if got := auxCompatModel(&auxBareClient{base: "https://api.gmi.ai/v1"}, "org/model", "plain"); got != "plain" {
		t.Fatalf("slash model kept on non openrouter %q", got)
	}
	if got := auxCompatModel(&auxBareClient{base: "https://openrouter.ai/api/v1"}, "org/model", "plain"); got != "org/model" {
		t.Fatalf("slash model dropped on openrouter %q", got)
	}
	if got := auxCompatModel(&auxBareClient{base: "https://api.gmi.ai/v1"}, "org/model", "vendor/default"); got != "org/model" {
		t.Fatalf("slash default should accept slash ids %q", got)
	}
}

func TestNormalizeVisionProviderMainResolvesToOpenRouter(t *testing.T) {
	ctx := auxMainTestHome(t, "model:\n  provider: openrouter\n  default: x\n")
	if got := auxNormalizeVisionProvider(ctx, "main"); got != "openrouter" {
		t.Fatalf("main resolved to %q", got)
	}
}

func TestPreserveProviderWithBaseURLRegistriesFailIndependently(t *testing.T) {
	ctx := auxMainTestHome(t, "providers:\n  my-gateway:\n    base_url: https://gw.example/v1\n")
	prevDef := auxGetProviderDefFn
	t.Cleanup(func() { auxGetProviderDefFn = prevDef })
	auxGetProviderDefFn = func(context.Context, string) *ProviderDef { panic(fmt.Errorf("catalog boom")) }
	if !auxPreserveProviderWithBaseURL(ctx, "my-gateway") {
		t.Fatal("user defined provider dropped when the builtin registry raised")
	}
	if !auxPreserveProviderWithBaseURL(ctx, "nous") {
		t.Fatal("hardcoded allowlist ignored when the catalog raised")
	}
	if auxPreserveProviderWithBaseURL(ctx, "custom") || auxPreserveProviderWithBaseURL(ctx, "custom:x") || !auxPreserveProviderWithBaseURL(ctx, "ollama") {
		t.Fatal("custom names or local aliases misclassified")
	}
	prevNamed := auxNamedCustomProviderFn
	t.Cleanup(func() { auxNamedCustomProviderFn = prevNamed })
	auxNamedCustomProviderFn = func(context.Context, string) bool { panic(fmt.Errorf("config boom")) }
	if auxPreserveProviderWithBaseURL(ctx, "my-gateway") {
		t.Fatal("both registries unavailable must fail closed")
	}
}

func TestUnwrapMoaProviderKeepsNonMoa(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	if p, m := auxUnwrapMoaProvider(ctx, "openrouter", "x"); p != "openrouter" || m != "x" {
		t.Fatalf("non moa changed %q %q", p, m)
	}
	if p, m := auxUnwrapMoaProvider(ctx, "moa", "missing-preset"); p != "moa" || m != "missing-preset" {
		t.Fatalf("unresolvable preset changed %q %q", p, m)
	}
}

// Async test client whose close is an awaitable and whose transport can be neutered
type auxAwaitableTestClient struct {
	auxBareClient
	awaited, neutered bool
}

func (c *auxAwaitableTestClient) CloseAwaitable() func() error {
	return func() error { c.awaited = true; return nil }
}
func (c *auxAwaitableTestClient) ForceCloseTransport() { c.neutered = true }

// Client recording whether the cache lock was free when it was closed
type auxLockProbeClient struct {
	auxBareClient
	observed []bool
}

func (c *auxLockProbeClient) Close() error {
	free := auxClientCacheMu.TryLock()
	if free {
		auxClientCacheMu.Unlock()
	}
	c.observed = append(c.observed, free)
	return nil
}
