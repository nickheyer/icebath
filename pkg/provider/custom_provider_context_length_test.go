package provider

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func customRoute(baseURL string, models map[string]any) []map[string]any {
	return []map[string]any{{"base_url": baseURL, "models": models}}
}

func TestGetCustomProviderContextLengthTrailingSlashInsensitive(t *testing.T) {
	custom := customRoute("https://example.invalid/v1/", map[string]any{"m": map[string]any{"context_length": 500_000}})
	if got := CustomProviderContextLength("m", "https://example.invalid/v1", custom, nil); got != 500_000 {
		t.Fatalf("config slash, runtime bare -> %d", got)
	}
	custom2 := customRoute("https://example.invalid/v1", map[string]any{"m": map[string]any{"context_length": 500_000}})
	if got := CustomProviderContextLength("m", "https://example.invalid/v1/", custom2, nil); got != 500_000 {
		t.Fatalf("config bare, runtime slash -> %d", got)
	}
}

func TestGetCustomProviderContextLengthEmptyInputsReturnNone(t *testing.T) {
	if CustomProviderContextLength("", "http://x", customRoute("http://x", map[string]any{"": map[string]any{"context_length": 1}}), nil) != 0 {
		t.Fatal("an empty model resolves nothing")
	}
	if CustomProviderContextLength("m", "", customRoute("", map[string]any{"m": map[string]any{"context_length": 1}}), nil) != 0 {
		t.Fatal("an empty base url resolves nothing")
	}
	if CustomProviderContextLength("m", "http://x", nil, nil) != 0 || CustomProviderContextLength("m", "http://x", []map[string]any{}, nil) != 0 {
		t.Fatal("no routes resolve nothing")
	}
}

func TestGetCustomProviderModelCapabilityMatchesExactModelOnNormalizedRoute(t *testing.T) {
	custom := customRoute("https://example.invalid/anthropic/", map[string]any{"fable": map[string]any{"prompt_caching": true}})
	if value, known := CustomProviderModelCapability("fable", "https://example.invalid/anthropic", "prompt_caching", custom, nil); !known || !value {
		t.Fatalf("fable -> %t %t", value, known)
	}
	if _, known := CustomProviderModelCapability("opus", "https://example.invalid/anthropic", "prompt_caching", custom, nil); known {
		t.Fatal("an undeclared model has no capability")
	}
}

func TestGetCustomProviderModelCapabilityFalseIsPreservedAndNonBooleanIsIgnored(t *testing.T) {
	custom := customRoute("https://example.invalid/anthropic", map[string]any{
		"disabled": map[string]any{"prompt_caching": false},
		"invalid":  map[string]any{"prompt_caching": "true"},
	})
	if value, known := CustomProviderModelCapability("disabled", "https://example.invalid/anthropic", "prompt_caching", custom, nil); !known || value {
		t.Fatalf("disabled -> %t %t", value, known)
	}
	if _, known := CustomProviderModelCapability("invalid", "https://example.invalid/anthropic", "prompt_caching", custom, nil); known {
		t.Fatal("a non boolean declaration is ignored")
	}
}

func TestGetCustomProviderModelCapabilityCapabilityIsRouteIsolated(t *testing.T) {
	custom := customRoute("https://other.example.invalid/anthropic", map[string]any{"fable": map[string]any{"prompt_caching": true}})
	if _, known := CustomProviderModelCapability("fable", "https://example.invalid/anthropic", "prompt_caching", custom, nil); known {
		t.Fatal("a declaration for one route must not apply to another")
	}
}

// Resolver context whose every probe is refused, the default fallback the only remaining source
func probelessResolver(t *testing.T) context.Context {
	t.Helper()
	ctx, ft, _ := metaTest(t)
	ft.handle = func(*http.Request) (*http.Response, error) { return nil, errRefused }
	return ctx
}

var errRefused = &refusedError{}

type refusedError struct{}

func (*refusedError) Error() string { return "connection refused" }

func TestGetModelContextLengthHonorsOverrideCustomProvidersOverrideWinsOverDefaultFallback(t *testing.T) {
	ctx := probelessResolver(t)
	custom := customRoute("https://example.invalid/v1", map[string]any{"gpt-5.5": map[string]any{"context_length": 1_050_000}})
	if got := GetModelContextLength(ctx, ContextLengthRequest{Model: "gpt-5.5", BaseURL: "https://example.invalid/v1", Provider: "custom", CustomProviders: custom}); got != 1_050_000 {
		t.Fatalf("got %d", got)
	}
}

func TestGetModelContextLengthHonorsOverrideExplicitConfigContextLengthStillWins(t *testing.T) {
	ctx := probelessResolver(t)
	custom := customRoute("https://example.invalid/v1", map[string]any{"m": map[string]any{"context_length": 1_050_000}})
	if got := GetModelContextLength(ctx, ContextLengthRequest{Model: "m", BaseURL: "https://example.invalid/v1", Provider: "custom", ConfigContextLength: 500_000, CustomProviders: custom}); got != 500_000 {
		t.Fatalf("got %d", got)
	}
}

func TestGetModelContextLengthHonorsOverrideNoOverrideFallsThroughToDefault(t *testing.T) {
	ctx := probelessResolver(t)
	if got := GetModelContextLength(ctx, ContextLengthRequest{Model: "unknown-model", BaseURL: "https://example.invalid/v1", Provider: "custom"}); got != DefaultFallbackContext {
		t.Fatalf("got %d want %d", got, DefaultFallbackContext)
	}
}

func TestOverrideHonoredWhenCallerPassesNoCustomProviders(t *testing.T) {
	ctx, _, dir := metaTest(t)
	baseURL, model, override := "https://cp-ctx-selfresolve.invalid/v1", "router/auto", 999_999
	cfg := "custom_providers:\n  - name: test-route\n    base_url: " + baseURL + "\n    key_env: TEST_FAKE_CONTEXT_ENV\n    model: " + model +
		"\n    api_mode: chat_completions\n    models:\n      " + model + ":\n        context_length: 999999\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := GetModelContextLength(ctx, ContextLengthRequest{Model: model, BaseURL: baseURL, Provider: "custom"}); got != override {
		t.Fatalf("got %d", got)
	}
}

func TestEntryLevelCustomProviderContextLengthBacksModelsWithoutPerModelOverride(t *testing.T) {
	providers := []map[string]any{{"name": "relay", "base_url": "https://relay.example/v1", "context_length": 1_000_000,
		"models": map[string]any{"pinned": map[string]any{"context_length": 65536}}}}
	if got := CustomProviderContextLength("pinned", "https://relay.example/v1", providers, nil); got != 65536 {
		t.Fatalf("pinned -> %d", got)
	}
	if got := CustomProviderContextLength("z-ai-glm-5-3", "https://relay.example/v1", providers, nil); got != 1_000_000 {
		t.Fatalf("entry level -> %d", got)
	}
	if got := CustomProviderContextLength("x", "https://other.example/v1", providers, nil); got != 0 {
		t.Fatalf("other route -> %d", got)
	}
}

func TestEndpointModelCatalogMemoIsKeyedByCredential(t *testing.T) {
	_, ft, root := metaTest(t)
	ft.handle = func(r *http.Request) (*http.Response, error) {
		parts := strings.Split(r.Header.Get("Authorization"), "-")
		who := parts[len(parts)-1]
		return jsonResponse(nil, 200, map[string]any{"data": []any{map[string]any{"id": "model-for-" + who, "context_length": 1}}}, nil), nil
	}
	ctxA := home.WithOverride(context.Background(), root)
	profileB := filepath.Join(root, "profiles", "B")
	if err := os.MkdirAll(profileB, 0o755); err != nil {
		t.Fatal(err)
	}
	ctxB := home.WithOverride(context.Background(), profileB)
	keysOf := func(catalog map[string]map[string]any) string {
		keys := sortedStringKeys(catalog)
		return strings.Join(keys, ",")
	}
	if got := keysOf(FetchEndpointModelMetadata(ctxA, "http://gw.example/v1", "key-A", false)); got != "model-for-A" {
		t.Fatalf("profile A got %q", got)
	}
	if got := keysOf(FetchEndpointModelMetadata(ctxB, "http://gw.example/v1", "key-B", false)); got != "model-for-B" {
		t.Fatalf("profile B got %q", got)
	}
	if got := keysOf(FetchEndpointModelMetadata(ctxA, "http://gw.example/v1", "key-A", false)); got != "model-for-A" {
		t.Fatalf("profile A again got %q", got)
	}
}
