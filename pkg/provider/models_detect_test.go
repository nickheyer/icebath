package provider

import (
	"context"
	"testing"
)

// Live catalogs pinned per provider plus a loud OpenRouter lookup
func modelsDetectFixture(t *testing.T) (context.Context, map[string][]string, map[string]bool) {
	t.Helper()
	ctx := modelsTestHome(t)
	catalogs := map[string][]string{}
	granted := map[string]bool{}
	cachedProviderModelIDsFn = func(_ context.Context, provider string, _ CachedModelsOptions) []string {
		return cloneStrings(catalogs[provider])
	}
	findOpenRouterSlugFn = func(_ context.Context, name string) string { return "vendor/" + name }
	providerHasCredentialsFn = func(_ context.Context, p string) bool { return granted[p] }
	return ctx, catalogs, granted
}

func TestNoCredentialsNoSwitch(t *testing.T) {
	ctx, _, granted := modelsDetectFixture(t)
	if _, _, ok := DetectProviderForModel(ctx, "some-model-only-openrouter-has", "deepseek"); ok {
		t.Fatal("openrouter only model stays when no openrouter key")
	}
	granted["openrouter"] = true
	if p, m, ok := DetectProviderForModel(ctx, "some-model-only-openrouter-has", "deepseek"); !ok || p != "openrouter" || m != "vendor/some-model-only-openrouter-has" {
		t.Fatalf("openrouter remap allowed with a key: %q %q", p, m)
	}
	delete(granted, "openrouter")
	detectStaticProviderForModelFn = func(_ context.Context, n, _ string) (string, string, bool) { return "anthropic", n, true }
	if _, _, ok := DetectProviderForModel(ctx, "claude-something", "deepseek"); ok {
		t.Fatal("a static vendor match requires that vendor's credentials")
	}
	granted["anthropic"] = true
	if p, m, ok := DetectProviderForModel(ctx, "claude-something", "deepseek"); !ok || p != "anthropic" || m != "claude-something" {
		t.Fatalf("credentialed vendor: %q %q", p, m)
	}
	delete(granted, "anthropic")
	detectStaticProviderForModelFn = func(context.Context, string, string) (string, string, bool) { return "nous", "hermes-4-405b", true }
	if p, m, ok := DetectProviderForModel(ctx, "nous", "deepseek"); !ok || p != "nous" || m != "hermes-4-405b" {
		t.Fatalf("an explicitly named provider is not gated: %q %q", p, m)
	}
}

func TestSharedSlugTiebreak(t *testing.T) {
	ctx, _, granted := modelsDetectFixture(t)
	granted["openai-codex"] = true
	for _, current := range []string{"deepseek", "auto"} {
		if p, m, ok := DetectProviderForModel(ctx, "gpt-5.6-luna", current); !ok || p != "openai-codex" || m != "gpt-5.6-luna" {
			t.Fatalf("%s: shared slug goes to the credentialed sibling: %q %q", current, p, m)
		}
	}
	granted["openai-api"] = true
	if p, _, ok := DetectProviderForModel(ctx, "gpt-5.6-luna", "deepseek"); !ok || p != "openai-api" {
		t.Fatalf("first catalog kept when usable: %q", p)
	}
	delete(granted, "openai-api")
	delete(granted, "openai-codex")
	if p, _, ok := DetectProviderForModel(ctx, "gpt-5.6-luna", "auto"); !ok || p != "openai-api" {
		t.Fatalf("a fresh session fails loudly on the first guess: %q", p)
	}
}

func TestCurrentProviderCatalogWins(t *testing.T) {
	ctx, catalogs, granted := modelsDetectFixture(t)
	for _, c := range [][2]string{{"openai-codex", "gpt-6-astra"}, {"nous", "some-portal-only-model"}, {"ollama-cloud", "glm-5.3-flash"}} {
		catalogs[c[0]] = []string{c[1]}
		if p, m, ok := DetectProviderForModel(ctx, c[1], c[0]); ok {
			t.Fatalf("served model stays on %s: %q %q", c[0], p, m)
		}
	}
	catalogs["nous"] = []string{"zai/glm-5.3-flash"}
	if p, m, ok := DetectProviderForModel(ctx, "glm-5.3-flash", "nous"); !ok || p != "nous" || m != "zai/glm-5.3-flash" {
		t.Fatalf("bare name resolves to the current provider's full slug: %q %q", p, m)
	}
	granted["openrouter"] = true
	catalogs["nous"] = []string{"hermes-4-405b"}
	if p, m, ok := DetectProviderForModel(ctx, "no-such-model", "nous"); !ok || p != "openrouter" || m != "vendor/no-such-model" {
		t.Fatalf("unserved model still walks the ladder: %q %q", p, m)
	}
}

func TestOwnVendorIDStaysWhenLiveCatalogIsEmpty(t *testing.T) {
	ctx, _, granted := modelsDetectFixture(t)
	granted["openrouter"] = true
	for _, c := range [][2]string{{"openai-codex", "gpt-6-astra"}, {"xai-oauth", "grok-5-preview"}, {"anthropic", "claude-opus-5-early"}} {
		if p, m, ok := DetectProviderForModel(ctx, c[1], c[0]); ok {
			t.Fatalf("%s owns %s: %q %q", c[0], c[1], p, m)
		}
	}
	for _, c := range [][2]string{{"openai-codex", "claude-opus-4.7"}, {"bedrock", "deepseek-v4-pro"}} {
		if p, m, ok := DetectProviderForModel(ctx, c[1], c[0]); !ok || p != "openrouter" || m != "vendor/"+c[1] {
			t.Fatalf("non owned id remaps to the keyed aggregator: %q %q %v", p, m, ok)
		}
	}
}

func TestProviderHasCredentials(t *testing.T) {
	ctx := modelsTestHome(t)
	if !ProviderHasCredentials(ctx, "custom") || !ProviderHasCredentials(ctx, "custom:local") || ProviderHasCredentials(ctx, "") {
		t.Fatal("custom targets count as authenticated, empty never")
	}
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-abcdefghijklmnopqrstuvwxyz0123456789")
	if !ProviderHasCredentials(ctx, "openrouter") {
		t.Fatal("an openrouter key in the environment counts")
	}
	providerAuthStatusFn = func(_ context.Context, p string) (bool, bool) { return false, p == "deepseek" }
	if !ProviderHasCredentials(ctx, "deepseek") || ProviderHasCredentials(ctx, "anthropic") {
		t.Fatal("auth status configured counts, an empty pool does not")
	}
}
