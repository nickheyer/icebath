package provider

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Entra credential minting jwt-for-<scope> like the hermes fake azure.identity module
type auxFakeEntraCredential struct{}

func (auxFakeEntraCredential) GetToken(_ context.Context, scopes ...string) (EntraAccessToken, error) {
	scope := ""
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	return EntraAccessToken{Token: "jwt-for-" + scope, ExpiresOn: 9999999999}, nil
}

func (auxFakeEntraCredential) Name() string { return "fake-azure-identity" }

// Installs the fake Entra credential chain and clears the credential cache around the test
func auxFakeAzureIdentity(t *testing.T) {
	t.Helper()
	ResetEntraCredentialCache()
	prev := entraBuildDefaultFn
	entraBuildDefaultFn = func(EntraIdentityConfig) (entraCredential, error) { return auxFakeEntraCredential{}, nil }
	t.Cleanup(func() {
		entraBuildDefaultFn = prev
		ResetEntraCredentialCache()
	})
}

// Temp home plus the model config the hermes patch_load_config fixture installs
func auxAzureFoundryConfig(t *testing.T, modelCfg map[string]any) context.Context {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ResetEntraCredentialCache()
	t.Cleanup(ResetEntraCredentialCache)
	auxFakeConfigTree(t, config.Tree{"model": modelCfg})
	return context.Background()
}

// Azure Foundry model config over the OpenAI v1 endpoint with the extra keys applied
func auxAzureOpenAIModelCfg(extra map[string]any) map[string]any {
	cfg := map[string]any{"provider": "azure-foundry", "base_url": "https://r.openai.azure.com/openai/v1"}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

// Static key or callable of an OpenAI client, the callable's token minted
func auxOpenAIClientKey(t *testing.T, client any) (string, func() (string, error)) {
	t.Helper()
	oc, ok := client.(*auxOpenAIClient)
	if !ok {
		t.Fatalf("client type %T", client)
	}
	return oc.apiKey, oc.apiKeyProvider
}

// Asserts the client carries an Entra callable whose token starts with jwt-for- and never a static key
func auxAssertEntraCallable(t *testing.T, client any) {
	t.Helper()
	static, provider := auxOpenAIClientKey(t, client)
	if provider == nil || static != "" {
		t.Fatalf("the Entra callable must reach the OpenAI client as a callable, static key %q", static)
	}
	token, err := provider()
	if err != nil || !strings.HasPrefix(token, "jwt-for-") {
		t.Fatalf("token %q err %v", token, err)
	}
}

func TestAuxAzureFoundryApiKeyChatCompletionsReturnsPlainOpenaiClient(t *testing.T) {
	t.Setenv("AZURE_FOUNDRY_API_KEY", "sk-azure-static-key")
	ctx := auxAzureFoundryConfig(t, auxAzureOpenAIModelCfg(map[string]any{"api_mode": "chat_completions", "default": "gpt-4o"}))
	client, resolved, err := auxTryAzureFoundry(ctx, "gpt-4o", nil, "", "")
	if err != nil || client == nil || resolved != "gpt-4o" {
		t.Fatalf("client %v resolved %q err %v", client, resolved, err)
	}
	if static, _ := auxOpenAIClientKey(t, client); static != "sk-azure-static-key" {
		t.Fatalf("api key %q", static)
	}
}

func TestAuxAzureFoundryApiKeyNoKeyReturnsNone(t *testing.T) {
	t.Setenv("AZURE_FOUNDRY_API_KEY", "")
	os.Unsetenv("AZURE_FOUNDRY_API_KEY")
	ctx := auxAzureFoundryConfig(t, auxAzureOpenAIModelCfg(map[string]any{"api_mode": "chat_completions", "default": "gpt-4o"}))
	client, resolved, err := auxTryAzureFoundry(ctx, "gpt-4o", nil, "", "")
	if err != nil || client != nil || resolved != "" {
		t.Fatalf("client %v resolved %q err %v", client, resolved, err)
	}
}

func TestAuxAzureFoundryEntraCallableApiKeyReachesOpenaiConstructor(t *testing.T) {
	auxFakeAzureIdentity(t)
	ctx := auxAzureFoundryConfig(t, auxAzureOpenAIModelCfg(map[string]any{"api_mode": "chat_completions", "auth_mode": "entra_id", "default": "gpt-4o"}))
	client, resolved, err := auxTryAzureFoundry(ctx, "gpt-4o", nil, "", "")
	if err != nil || client == nil || resolved != "gpt-4o" {
		t.Fatalf("client %v resolved %q err %v", client, resolved, err)
	}
	auxAssertEntraCallable(t, client)
	if got := client.BaseURL(); strings.TrimSuffix(got, "/") != "https://r.openai.azure.com/openai/v1" {
		t.Fatalf("base url %q", got)
	}
}

func TestAuxAzureFoundryEntraCodexResponsesWithEntraWrapsCorrectly(t *testing.T) {
	auxFakeAzureIdentity(t)
	ctx := auxAzureFoundryConfig(t, auxAzureOpenAIModelCfg(map[string]any{"api_mode": "chat_completions", "auth_mode": "entra_id", "default": "gpt-5.4-mini"}))
	client, resolved, err := auxTryAzureFoundry(ctx, "gpt-5.4-mini", nil, "", "")
	if err != nil || resolved != "gpt-5.4-mini" {
		t.Fatalf("resolved %q err %v", resolved, err)
	}
	codex, ok := client.(*CodexAuxiliaryClient)
	if !ok {
		t.Fatalf("client type %T", client)
	}
	auxAssertEntraCallable(t, codex.realClient)
}

func TestAuxAzureFoundryEntraEntraAnthropicMessagesUsesBearerHook(t *testing.T) {
	auxFakeAzureIdentity(t)
	ctx := auxAzureFoundryConfig(t, map[string]any{"provider": "azure-foundry", "base_url": "https://r.services.ai.azure.com/anthropic",
		"api_mode": "anthropic_messages", "auth_mode": "entra_id", "default": "claude-sonnet-4-5"})
	client, resolved, err := auxTryAzureFoundry(ctx, "claude-sonnet-4-5", nil, "", "")
	if err != nil || client == nil || resolved != "claude-sonnet-4-5" {
		t.Fatalf("client %v resolved %q err %v", client, resolved, err)
	}
	anthropic, ok := client.(*AnthropicAuxiliaryClient)
	if !ok {
		t.Fatalf("client type %T", client)
	}
	route := anthropic.realClient.inner.route
	if route.TokenSource == nil || route.APIKey != "" {
		t.Fatalf("the Anthropic client must mint bearer tokens per request through the Entra callable, static key %q", route.APIKey)
	}
	token, err := route.TokenSource(ctx)
	if err != nil || !strings.HasPrefix(token, "jwt-for-") {
		t.Fatalf("token %q err %v", token, err)
	}
	if _, isBearer := anthropic.realClient.inner.httpClient(10 * time.Second).Transport.(*EntraBearerTransport); !isBearer {
		t.Fatalf("transport %T must inject the bearer per request", anthropic.realClient.inner.httpClient(10*time.Second).Transport)
	}
}

func TestResolveProviderClientAzureFoundryDispatchesToAzureBranchNotGenericApiKeyPath(t *testing.T) {
	auxFakeAzureIdentity(t)
	ctx := auxAzureFoundryConfig(t, auxAzureOpenAIModelCfg(map[string]any{"api_mode": "chat_completions", "auth_mode": "entra_id", "default": "gpt-4o"}))
	client, resolved, err := ResolveProviderClient(ctx, "azure-foundry", "gpt-4o", ProviderClientOptions{})
	if err != nil || client == nil || resolved != "gpt-4o" {
		t.Fatalf("client %v resolved %q err %v", client, resolved, err)
	}
	if _, provider := auxOpenAIClientKey(t, client); provider == nil {
		t.Fatal("the Entra callable did not reach the OpenAI client through resolve_provider_client")
	}
}

func TestResolveProviderClientAzureFoundryWarnsAndReturnsNoneOnFailure(t *testing.T) {
	t.Setenv("AZURE_FOUNDRY_API_KEY", "")
	os.Unsetenv("AZURE_FOUNDRY_API_KEY")
	ctx := auxAzureFoundryConfig(t, auxAzureOpenAIModelCfg(map[string]any{"api_mode": "chat_completions"}))
	logs := auxCaptureLogs(t)
	client, resolved, err := ResolveProviderClient(ctx, "azure-foundry", "", ProviderClientOptions{})
	if err != nil || client != nil || resolved != "" {
		t.Fatalf("client %v resolved %q err %v", client, resolved, err)
	}
	warned := false
	for _, rec := range logs.matching("azure-foundry") {
		warned = warned || rec.level >= slog.LevelWarn && strings.Contains(rec.msg, "icebath doctor")
	}
	if !warned {
		t.Fatalf("no azure-foundry doctor warning in %v", logs.records)
	}
}

func TestAzureFoundryResponsesAliasTaskLevelResponsesAliasRoutesVisionThroughResponsesAdapter(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("AZURE_FOUNDRY_API_KEY", "k")
	auxFakeConfigTree(t, config.Tree{"model": map[string]any{"provider": "openrouter", "default": "x"},
		"auxiliary": map[string]any{"vision": map[string]any{"provider": "azure-foundry", "model": "gpt-5.4-nano",
			"base_url": "https://r.services.ai.azure.com/openai/v1", "api_mode": "responses"}}})
	provider, client, model, err := ResolveVisionProviderClient(context.Background(), "", "", "", nil, false, nil)
	if err != nil || provider != "azure-foundry" || model != "gpt-5.4-nano" {
		t.Fatalf("provider %q model %q err %v", provider, model, err)
	}
	if _, ok := client.(*CodexAuxiliaryClient); !ok {
		t.Fatalf("client type %T", client)
	}
}

func TestAzureFoundryResponsesAliasExplicitResponsesAliasKwargWrapsInCodexAdapter(t *testing.T) {
	t.Setenv("AZURE_FOUNDRY_API_KEY", "k")
	ctx := auxAzureFoundryConfig(t, map[string]any{"provider": "azure-foundry", "base_url": "https://r.services.ai.azure.com/openai/v1"})
	client, model, err := ResolveProviderClient(ctx, "azure-foundry", "gpt-5.4-nano", ProviderClientOptions{APIMode: "responses"})
	if err != nil || model != "gpt-5.4-nano" {
		t.Fatalf("model %q err %v", model, err)
	}
	if _, ok := client.(*CodexAuxiliaryClient); !ok {
		t.Fatalf("client type %T", client)
	}
}
