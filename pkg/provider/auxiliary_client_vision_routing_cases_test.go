package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

const auxOpenAIAliasVisionYAML = "auxiliary:\n  vision:\n    provider: openai\n    model: gpt-4o-mini\n"

// Temp home with the config, every credential env var and the module level models.dev registry cleared
func auxVisionIsolatedHome(t *testing.T, configYAML string) context.Context {
	t.Helper()
	ResetModelsDevCache()
	t.Cleanup(ResetModelsDevCache)
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && (strings.HasSuffix(k, "_API_KEY") || strings.HasSuffix(k, "_TOKEN")) {
			t.Setenv(k, v)
			os.Unsetenv(k)
		}
	}
	return auxRecoveryHome(t, configYAML)
}

func TestOpenAiAliasForAuxiliaryProviderOpenaiRoutesToOpenaiDotCom(t *testing.T) {
	ctx := auxVisionIsolatedHome(t, auxOpenAIAliasVisionYAML)
	t.Setenv("OPENAI_API_KEY", "sk-test")
	route := auxResolveTaskProviderModel(ctx, "vision", "", "", "", nil)
	if route.Provider != "custom" || route.Model != "gpt-4o-mini" || route.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("route %+v", route)
	}
}

func TestOpenAiAliasForAuxiliaryProviderOpenaiWithExplicitBaseUrlPreservesUserEndpoint(t *testing.T) {
	ctx := auxVisionIsolatedHome(t, auxOpenAIAliasVisionYAML+"    base_url: https://my-proxy.example.com/v1\n")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	route := auxResolveTaskProviderModel(ctx, "vision", "", "", "", nil)
	if route.Provider != "custom" || route.BaseURL != "https://my-proxy.example.com/v1" {
		t.Fatalf("route %+v", route)
	}
}

func TestOpenAiAliasForAuxiliaryProviderOpenaiResolvesToWorkingClient(t *testing.T) {
	ctx := auxVisionIsolatedHome(t, auxOpenAIAliasVisionYAML)
	t.Setenv("OPENAI_API_KEY", "sk-test")
	_, client, model, err := ResolveVisionProviderClient(ctx, "", "", "", nil, false, nil)
	if err != nil || client == nil {
		t.Fatalf("openai alias should produce a usable client, got %v %v", client, err)
	}
	parsed, err := url.Parse(client.BaseURL())
	if err != nil || parsed.Hostname() != "api.openai.com" {
		t.Fatalf("expected api.openai.com host, got %q", client.BaseURL())
	}
	if model != "gpt-4o-mini" {
		t.Fatalf("model %q", model)
	}
}

func TestTextOnlyMainSkippedForVisionTextOnlyMainSkippedWhenNoAggregator(t *testing.T) {
	ctx := auxVisionIsolatedHome(t, "model:\n  provider: deepseek\n  default: deepseek-v4-pro\n"+auxModelsDevYAML(auxVisionCatalogServer(t)))
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	provider, client, _, err := ResolveVisionProviderClient(ctx, "auto", "", "", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if client != nil {
		t.Fatalf("vision auto-detect must skip text-only main %q when no vision-capable aggregator is available", provider)
	}
}

func TestTextOnlyMainSkippedForVisionVisionCapableMainUsed(t *testing.T) {
	ctx := auxVisionIsolatedHome(t, "model:\n  provider: anthropic\n  default: claude-sonnet-4-6\n"+auxModelsDevYAML(auxVisionCatalogServer(t)))
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	provider, client, _, err := ResolveVisionProviderClient(ctx, "auto", "", "", nil, false, nil)
	if err != nil || client == nil || provider != "anthropic" {
		t.Fatalf("provider %q client %v err %v", provider, client, err)
	}
}

func TestTextOnlyMainSkippedForVisionUnknownCapabilityDoesNotBlock(t *testing.T) {
	if !auxMainModelSupportsVision(auxVisionIsolatedHome(t, ""), "nonexistent-provider", "nonexistent-model") {
		t.Fatal("unknown capability must fall back to permissive")
	}
}

// models.dev catalog server knowing deepseek-v4-pro as text only and claude-sonnet-4-6 as vision capable
func auxVisionCatalogServer(t *testing.T) string {
	t.Helper()
	entry := func(id string, inputs []any, attachment bool) map[string]any {
		return map[string]any{"id": id, "name": id, "attachment": attachment, "reasoning": false, "tool_call": true,
			"modalities": map[string]any{"input": inputs, "output": []any{"text"}}, "limit": map[string]any{"context": 128000, "output": 8192}}
	}
	catalog := map[string]any{
		"deepseek":  map[string]any{"id": "deepseek", "models": map[string]any{"deepseek-v4-pro": entry("deepseek-v4-pro", []any{"text"}, false)}},
		"anthropic": map[string]any{"id": "anthropic", "models": map[string]any{"claude-sonnet-4-6": entry("claude-sonnet-4-6", []any{"text", "image"}, true)}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(catalog)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func auxModelsDevYAML(url string) string { return "models_dev:\n  url: " + url + "\n" }

func TestVisionCapabilitiesServeTheToolsProbes(t *testing.T) {
	ctx := auxVisionIsolatedHome(t, auxModelsDevYAML(auxVisionCatalogServer(t)))
	if supported, known := (VisionCapabilities{}).CatalogSupportsVision(ctx, "deepseek", "deepseek-v4-pro"); supported || !known {
		t.Fatalf("deepseek-v4-pro verdict %v %v", supported, known)
	}
	if supported, known := (VisionCapabilities{}).CatalogSupportsVision(ctx, "anthropic", "claude-sonnet-4-6"); !supported || !known {
		t.Fatalf("claude-sonnet-4-6 verdict %v %v", supported, known)
	}
	if _, known := (VisionCapabilities{}).CatalogSupportsVision(ctx, "nonexistent-provider", "nonexistent-model"); known {
		t.Fatal("an uncatalogued model has no verdict")
	}
	registered, _ := GetProviderProfile(ctx, "anthropic")
	if vision, toolMessages, found := (VisionCapabilities{}).ProviderVisionProfile("anthropic"); !found || vision != registered.Vision || toolMessages != registered.VisionToolMessages {
		t.Fatalf("anthropic profile %v %v found %v", vision, toolMessages, found)
	}
	if _, _, found := (VisionCapabilities{}).ProviderVisionProfile("no-such-provider-xyz"); found {
		t.Fatal("an unregistered provider has no profile")
	}
	if (VisionCapabilities{}).StripCodexContextVariantSuffix("gpt-5.5") != StripCodexContextVariantSuffix("gpt-5.5") {
		t.Fatal("codex suffix strip must match the metadata helper")
	}
}
