package provider

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

const auxZaiMainConfigYAML = "model:\n  default: zai/glm-5.1\n  provider: zai\n"

const auxOpenrouterMainConfigYAML = "model:\n  provider: openrouter\n  default: anthropic/claude-sonnet-4.6\n"

// Swaps the API key provider credential resolver for the static key and base URL
func auxFakeAPIKeyProviderCredentials(t *testing.T, key, baseURL string) {
	t.Helper()
	prev := auxResolveAPIKeyProviderCredentialsFn
	t.Cleanup(func() { auxResolveAPIKeyProviderCredentialsFn = prev })
	auxResolveAPIKeyProviderCredentialsFn = func(context.Context, string) (apiKeyCreds, error) {
		return apiKeyCreds{APIKey: key, BaseURL: baseURL}, nil
	}
}

// Removes the named environment variables for the test and restores them afterwards
func auxNamedCustomUnsetEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

// Header value under a case insensitive key lookup, empty when absent
func auxHeaderValue(headers map[string]string, key string) (string, bool) {
	for k, v := range headers {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

func TestResolveProviderClientModelNormalizationMatchingNativePrefixIsStrippedForMainProvider(t *testing.T) {
	ctx := auxMainTestHome(t, auxZaiMainConfigYAML)
	auxFakeAPIKeyProviderCredentials(t, "glm-key", "https://api.z.ai/api/paas/v4")
	client, model, err := ResolveProviderClient(ctx, "main", "zai/glm-5.1", ProviderClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || model != "glm-5.1" {
		t.Fatalf("client %v model %q", client, model)
	}
}

func TestResolveProviderClientModelNormalizationAggregatorVendorSlugIsPreserved(t *testing.T) {
	ctx := auxMainTestHome(t, "model:\n  default: test-model\n")
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	client, model, err := ResolveProviderClient(ctx, "openrouter", "anthropic/claude-sonnet-4.6", ProviderClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || model != "anthropic/claude-sonnet-4.6" {
		t.Fatalf("client %v model %q", client, model)
	}
}

func TestResolveVisionProviderClientModelNormalizationVisionAutoStripsMatchingMainProviderPrefix(t *testing.T) {
	ctx := auxMainTestHome(t, auxZaiMainConfigYAML)
	auxFakeAPIKeyProviderCredentials(t, "glm-key", "https://api.z.ai/api/paas/v4")
	provider, client, model, err := ResolveVisionProviderClient(ctx, "", "", "", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if provider != "zai" || client == nil || model != "glm-5.3-flash" {
		t.Fatalf("provider %q client %v model %q", provider, client, model)
	}
}

func TestAutoClientCacheModelCompatibilityFirstAutoCacheMissDropsOpenrouterModelForNamedCustomRuntime(t *testing.T) {
	ctx := auxMainTestHome(t, "model:\n  default: test-model\n")
	ShutdownCachedClients(ctx)
	t.Cleanup(func() { ShutdownCachedClients(ctx) })
	fake := &auxCallFakeClient{base: "https://aixj.vip/v1", key: "test-key",
		create: func(context.Context, map[string]any) (any, error) { return auxCallOK("ok"), nil }}
	runtime := map[string]any{"provider": "custom:aixj.vip", "model": "gpt-5.4", "base_url": "https://aixj.vip/v1", "api_key": "***", "api_mode": "codex_responses"}
	var calls atomic.Int32
	prev := auxResolveProviderClientFn
	t.Cleanup(func() { auxResolveProviderClientFn = prev })
	auxResolveProviderClientFn = func(context.Context, string, string, ProviderClientOptions) (AuxClient, string, error) {
		calls.Add(1)
		return fake, "gpt-5.4", nil
	}
	client, model, err := auxGetCachedClient(ctx, auxCachedClientRequest{Provider: "auto", Model: "google/gemini-3-flash-preview", MainRuntime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	if client != AuxClient(fake) || model != "gpt-5.4" {
		t.Fatalf("client %v model %q", client, model)
	}
	if calls.Load() != 1 {
		t.Fatalf("resolve calls %d", calls.Load())
	}
}

func TestProvidersDictApiModeAnthropicMessagesResolveProviderClientReturnsAnthropicClient(t *testing.T) {
	t.Setenv("MYRELAY_API_KEY", "sk-test")
	ctx := auxMainTestHome(t, "providers:\n  myrelay:\n    name: myrelay\n    base_url: https://example-relay.test/anthropic\n    key_env: MYRELAY_API_KEY\n    api_mode: anthropic_messages\n    default_model: claude-opus-4-7\n    extra_headers:\n      X-Gateway-Token: gw-1\n")
	syncClient, syncModel, err := ResolveProviderClient(ctx, "myrelay", "", ProviderClientOptions{AsyncMode: false})
	if err != nil {
		t.Fatal(err)
	}
	anth, ok := syncClient.(*AnthropicAuxiliaryClient)
	if !ok {
		t.Fatalf("expected AnthropicAuxiliaryClient, got %T", syncClient)
	}
	if syncModel != "claude-opus-4-7" {
		t.Fatalf("model %q", syncModel)
	}
	sdkHeaders, _, err := anth.realClient.inner.requestHeaders(ctx, anth.realClient.betas, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := auxHeaderValue(sdkHeaders, "X-Gateway-Token"); v != "gw-1" {
		t.Fatalf("headers %v", sdkHeaders)
	}
	if _, has := auxHeaderValue(sdkHeaders, "anthropic-beta"); !has {
		t.Fatalf("entry headers must merge onto, not replace, the builder's headers: %v", sdkHeaders)
	}
	asyncClient, asyncModel, err := ResolveProviderClient(ctx, "myrelay", "", ProviderClientOptions{AsyncMode: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := asyncClient.(*AsyncAnthropicAuxiliaryClient); !ok {
		t.Fatalf("expected AsyncAnthropicAuxiliaryClient, got %T", asyncClient)
	}
	if asyncModel != "claude-opus-4-7" {
		t.Fatalf("model %q", asyncModel)
	}
}

func TestCustomProviderAliasCollisionCustomNamedKimiWinsOverBuiltinAlias(t *testing.T) {
	ctx := auxMainTestHome(t, auxOpenrouterMainConfigYAML+"custom_providers:\n  - name: kimi\n    base_url: https://my-custom-kimi.example.com/v1\n    api_key: my-kimi-key\n    models:\n      my-kimi-model:\n        context_length: 200000\n")
	client, model, err := ResolveProviderClient(ctx, "kimi", "my-kimi-model", ProviderClientOptions{RawCodex: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.(*auxOpenAIClient); !ok {
		t.Fatalf("expected OpenAI client, got %T", client)
	}
	if !strings.Contains(client.BaseURL(), "my-custom-kimi.example.com") || client.APIKey() != "my-kimi-key" || model != "my-kimi-model" {
		t.Fatalf("base %q key %q model %q", client.BaseURL(), client.APIKey(), model)
	}
}

func TestCustomProviderAliasCollisionBareKimiWithoutCustomStillRoutesToBuiltin(t *testing.T) {
	ctx := auxMainTestHome(t, auxOpenrouterMainConfigYAML)
	t.Setenv("KIMI_API_KEY", "builtin-kimi-key")
	client, _, err := ResolveProviderClient(ctx, "kimi", "kimi-k2-0905-preview", ProviderClientOptions{RawCodex: true})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("client is nil")
	}
	if base := client.BaseURL(); !strings.Contains(base, "moonshot") && !strings.Contains(base, "kimi") {
		t.Fatalf("unexpected base_url %q", base)
	}
}

func TestCustomProviderAliasCollisionNamedLlamacppWinsOverLocalServerAlias(t *testing.T) {
	for _, provider := range []string{"llamacpp", "custom:llamacpp"} {
		t.Run(provider, func(t *testing.T) {
			ctx := auxMainTestHome(t, auxOpenrouterMainConfigYAML+"providers:\n  llamacpp:\n    base_url: http://127.0.0.1:8081/v1\n    model: local-model\n")
			client, model, err := ResolveProviderClient(ctx, provider, "local-model", ProviderClientOptions{RawCodex: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := client.(*auxOpenAIClient); !ok {
				t.Fatalf("expected OpenAI client, got %T", client)
			}
			if strings.TrimRight(client.BaseURL(), "/") != "http://127.0.0.1:8081/v1" || model != "local-model" {
				t.Fatalf("base %q model %q", client.BaseURL(), model)
			}
		})
	}
}

func TestCustomProviderAliasCollisionExplicitOverridesAppliedOnApiKeyBranch(t *testing.T) {
	ctx := auxMainTestHome(t, auxOpenrouterMainConfigYAML)
	t.Setenv("KIMI_API_KEY", "builtin-kimi-key")
	client, _, err := ResolveProviderClient(ctx, "kimi-coding", "kimi-k2", ProviderClientOptions{RawCodex: true,
		ExplicitBaseURL: "https://override.example.com", ExplicitAPIKey: "override-key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.(*auxOpenAIClient); !ok {
		t.Fatalf("expected OpenAI client, got %T", client)
	}
	if !strings.Contains(client.BaseURL(), "override.example.com") || client.APIKey() != "override-key" {
		t.Fatalf("base %q key %q", client.BaseURL(), client.APIKey())
	}
}

func TestResolveProviderClientMainRuntimeCustomCustomProviderMainRuntimeUsedDirectly(t *testing.T) {
	ctx := auxMainTestHome(t, "model:\n  default: test-model\n")
	runtime := map[string]any{"provider": "custom", "base_url": "https://my-gateway.example.com/v1", "api_key": "***", "model": "glm-5.1"}
	client, model, err := ResolveProviderClient(ctx, "custom", "explicit-glm-5.1", ProviderClientOptions{MainRuntime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || model != "explicit-glm-5.1" {
		t.Fatalf("client %v model %q", client, model)
	}
	if !strings.Contains(client.BaseURL(), "my-gateway.example.com") || client.APIKey() != "***" {
		t.Fatalf("base %q key %q", client.BaseURL(), client.APIKey())
	}
}

func TestResolveProviderClientMainRuntimeCustomCustomProviderMainRuntimeNoCredentialsFallsThrough(t *testing.T) {
	ctx := auxMainTestHome(t, "model:\n  default: test-model\n")
	auxNamedCustomUnsetEnv(t, "OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENROUTER_API_KEY", "ANTHROPIC_API_KEY")
	client, _, err := ResolveProviderClient(ctx, "custom", "", ProviderClientOptions{MainRuntime: map[string]any{"api_key": "k", "base_url": ""}})
	if err != nil {
		t.Fatal(err)
	}
	if client != nil {
		t.Fatalf("client %v", client)
	}
}

func TestResolveProviderClientMainRuntimeCustomCustomProviderMainRuntimeRespectsExplicitBaseURL(t *testing.T) {
	ctx := auxMainTestHome(t, "model:\n  default: test-model\n")
	runtime := map[string]any{"base_url": "https://main-runtime.example.com/v1", "api_key": "sk-main", "model": "ignored-model"}
	client, model, err := ResolveProviderClient(ctx, "custom", "explicit-model", ProviderClientOptions{
		ExplicitBaseURL: "https://explicit.example.com/v1", ExplicitAPIKey: "sk-explicit", MainRuntime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || model != "explicit-model" {
		t.Fatalf("client %v model %q", client, model)
	}
	if !strings.Contains(client.BaseURL(), "explicit.example.com") || client.APIKey() != "sk-explicit" {
		t.Fatalf("base %q key %q", client.BaseURL(), client.APIKey())
	}
}
