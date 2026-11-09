package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// Swaps the builtin catalog and named custom lookups of the task route, nil keeping the current one
func auxSwapRouteRegistries(t *testing.T, builtin func(context.Context, string) *ProviderDef, named func(context.Context, string) bool) {
	t.Helper()
	prevDef, prevNamed := auxGetProviderDefFn, auxNamedCustomProviderFn
	if builtin != nil {
		auxGetProviderDefFn = builtin
	}
	if named != nil {
		auxNamedCustomProviderFn = named
	}
	t.Cleanup(func() { auxGetProviderDefFn, auxNamedCustomProviderFn = prevDef, prevNamed })
}

func auxCatalogRaises(context.Context, string) *ProviderDef {
	panic(errors.New("built-in catalog unavailable"))
}
func auxCatalogMisses(context.Context, string) *ProviderDef { return nil }
func auxNamedRaises(context.Context, string) bool           { panic(errors.New("catalog unavailable")) }

func TestFastModelTierBuiltinRegistryRaisesDoesNotSuppressNamedCustomLookup(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxSwapRouteRegistries(t, auxCatalogRaises, func(context.Context, string) bool { return true })
	route := auxResolveTaskProviderModel(ctx, "vision", "agnes-ai.cn", "agnes-2.5-flash", "https://api.agnes-ai.cn/v1", nil)
	if route.Provider != "agnes-ai.cn" {
		t.Fatalf("Built-in registry raising must not suppress the named-custom lookup; the user-defined provider must remain named (review feedback on #76602 — partial-load failure path), got %q", route.Provider)
	}
	if route.BaseURL != "https://api.agnes-ai.cn/v1" {
		t.Fatalf("base_url %q", route.BaseURL)
	}
}

func TestFastModelTierResolveVisionProviderClientPreservesNamedProviderViaConfig(t *testing.T) {
	ctx := auxMainTestHome(t, "model:\n  default: test-model\nproviders:\n  agnes-ai.cn:\n    name: AgnesAI\n    base_url: https://api.agnes-ai.cn/v1\n    api_key: sk-agnes-test\n    model: agnes-2.5-flash\n")
	provider, client, _, err := ResolveVisionProviderClient(ctx, "agnes-ai.cn", "agnes-2.5-flash", "https://api.agnes-ai.cn/v1", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if provider != "agnes-ai.cn" {
		t.Fatalf("resolve_vision_provider_client must preserve a named user-defined provider + explicit base_url rather than downgrading to 'custom' (issue #76602 repro), got %q", provider)
	}
	if client == nil || client.APIKey() != "sk-agnes-test" {
		t.Fatalf("client %v", client)
	}
}

func TestPreserveNamedCustomProviderWithBaseUrlUserDefinedProviderNamedInConfigIsPreserved(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxSwapRouteRegistries(t, auxCatalogMisses, func(context.Context, string) bool { return true })
	route := auxResolveTaskProviderModel(ctx, "vision", "agnes-ai.cn", "agnes-2.5-flash", "https://api.agnes-ai.cn/v1", nil)
	if route.Provider != "agnes-ai.cn" {
		t.Fatalf("User-defined provider from providers: section must be preserved instead of being downgraded to 'custom' (issue #76602), got %q", route.Provider)
	}
	if route.BaseURL != "https://api.agnes-ai.cn/v1" {
		t.Fatalf("base_url %q", route.BaseURL)
	}
}

func TestPreserveNamedCustomProviderWithBaseUrlBuiltInProviderWithBaseUrlStillPreserved(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxSwapRouteRegistries(t, func(context.Context, string) *ProviderDef { return &ProviderDef{} }, nil)
	route := auxResolveTaskProviderModel(ctx, "moa_reference", "anthropic", "claude-sonnet-4-6", "https://api.anthropic.com/v1", "sk-anthropic")
	if route.Provider != "anthropic" || route.BaseURL != "https://api.anthropic.com/v1" {
		t.Fatalf("route %+v", route)
	}
}

func TestPreserveNamedCustomProviderWithBaseUrlUnknownProviderWithBaseUrlFallsBackToCustomDowngrade(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxSwapRouteRegistries(t, auxCatalogMisses, func(context.Context, string) bool { return false })
	route := auxResolveTaskProviderModel(ctx, "vision", "some-unknown-gateway", "custom-model", "https://example.com/v1", "some-token")
	if route.Provider != "custom" || route.BaseURL != "https://example.com/v1" {
		t.Fatalf("route %+v", route)
	}
}

func TestPreserveNamedCustomProviderWithBaseUrlHardcodedAllowlistStillWorksWhenBothRegistriesUnavailable(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxSwapRouteRegistries(t, auxCatalogRaises, auxNamedRaises)
	route := auxResolveTaskProviderModel(ctx, "moa_reference", "xai-oauth", "grok-3", "https://api.x.ai/v1", "xai-token")
	if route.Provider != "xai-oauth" {
		t.Fatalf("route %+v", route)
	}
}

func TestVisionAutoSkipsKimiCodingKimiCodingSkippedFallsThroughToOpenrouter(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxClearUnhealthy(t)
	fakeOR := &auxCallFakeClient{base: "https://openrouter.ai/api/v1", key: "or-key"}
	var mu sync.Mutex
	var violations []string
	record := func(msg string) {
		mu.Lock()
		violations = append(violations, msg)
		mu.Unlock()
	}
	prevProv, prevModel, prevRPC, prevStrict := auxReadMainProvider, auxReadMainModel, auxResolveProviderClientFn, auxResolveStrictVisionBackendFn
	t.Cleanup(func() {
		auxReadMainProvider, auxReadMainModel, auxResolveProviderClientFn, auxResolveStrictVisionBackendFn = prevProv, prevModel, prevRPC, prevStrict
	})
	auxReadMainProvider = func(context.Context) string { return "kimi-coding" }
	auxReadMainModel = func(context.Context) string { return "kimi-code" }
	auxResolveProviderClientFn = func(context.Context, string, string, ProviderClientOptions) (AuxClient, string, error) {
		record("resolve_provider_client should NOT be called for kimi-coding on the vision auto path")
		return nil, "", errors.New("resolve_provider_client should NOT be called for kimi-coding on the vision auto path")
	}
	auxResolveStrictVisionBackendFn = func(_ context.Context, provider, _ string) (AuxClient, string, error) {
		switch provider {
		case "openrouter":
			return fakeOR, "google/gemini-3-flash-preview", nil
		case "nous":
			return nil, "", nil
		}
		msg := fmt.Sprintf("strict vision backend should not be called for %q when main provider is kimi-coding", provider)
		record(msg)
		return nil, "", errors.New(msg)
	}
	provider, client, model, err := ResolveVisionProviderClient(ctx, "", "", "", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(violations) > 0 {
		t.Fatal(violations)
	}
	if provider != "openrouter" || client != AuxClient(fakeOR) || model != "google/gemini-3-flash-preview" {
		t.Fatalf("got %q %v %q", provider, client, model)
	}
}
