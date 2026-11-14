package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Scripts the runtime provider resolver with a result or an error and records its requested provider and model
func scriptRuntimeProviderCall(t *testing.T, rt *provider.Runtime, err error) *[][2]string {
	t.Helper()
	var calls [][2]string
	prev := resolveRuntimeProviderFn
	resolveRuntimeProviderFn = func(_ context.Context, _ string, requested, targetModel, _, _ string) (*provider.Runtime, error) {
		calls = append(calls, [2]string{requested, targetModel})
		return rt, err
	}
	t.Cleanup(func() { resolveRuntimeProviderFn = prev })
	return &calls
}

func resolveCreds(t *testing.T, parent *AIAgent, cfg map[string]any) (map[string]any, error) {
	t.Helper()
	return resolveDelegationCredentials(tools.WithToolEnv(context.Background(), parent.env), cfg, parent)
}

func TestDelegationCredentialResolutionNoProviderReturnsNoneCredentials(t *testing.T) {
	creds, err := resolveCreds(t, mockDelegateParent(t, 0), map[string]any{"model": "", "provider": ""})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"provider", "base_url", "api_key", "api_mode", "model"} {
		if creds[key] != nil {
			t.Fatalf("%s = %v", key, creds[key])
		}
	}
}

func TestDelegationCredentialResolutionDirectEndpointUsesConfiguredBaseUrlAndApiKey(t *testing.T) {
	creds, err := resolveCreds(t, mockDelegateParent(t, 0), map[string]any{"model": "qwen2.5-coder", "provider": "openrouter", "base_url": "http://localhost:1234/v1", "api_key": "local-key"})
	if err != nil {
		t.Fatal(err)
	}
	if creds["model"] != "qwen2.5-coder" || creds["provider"] != "custom" || creds["base_url"] != "http://localhost:1234/v1" || creds["api_key"] != "local-key" || creds["api_mode"] != "chat_completions" {
		t.Fatalf("creds %v", creds)
	}
}

func TestDelegationCredentialResolutionDirectEndpointAutoDetectsAnthropicMessagesSuffix(t *testing.T) {
	creds, err := resolveCreds(t, mockDelegateParent(t, 0), map[string]any{"model": "claude-opus-4-6", "provider": "custom", "base_url": "https://myfoundry.services.ai.azure.com/anthropic", "api_key": "foundry-key"})
	if err != nil {
		t.Fatal(err)
	}
	if creds["provider"] != "custom" || creds["base_url"] != "https://myfoundry.services.ai.azure.com/anthropic" || creds["api_key"] != "foundry-key" || creds["api_mode"] != "anthropic_messages" {
		t.Fatalf("creds %v", creds)
	}
}

func TestDelegationCredentialResolutionBaseUrlWithProviderCarriesRuntimeRequestOverrides(t *testing.T) {
	scriptRuntimeProviderCall(t, &provider.Runtime{Provider: "custom", BaseURL: "https://provider-default.example/v1", APIKey: "provider-key", APIMode: "chat_completions",
		RequestOverrides: map[string]any{"extra_body": map[string]any{"thinking": map[string]any{"type": "disabled"}}}}, nil)
	creds, err := resolveCreds(t, mockDelegateParent(t, 0), map[string]any{"model": "mimo-v2.5-pro", "provider": "mimo", "base_url": "https://api.xiaomimimo.com/v1", "api_key": "cfg-key"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"extra_body": map[string]any{"thinking": map[string]any{"type": "disabled"}}}
	if _, has := creds["max_output_tokens"]; has || creds["base_url"] != "https://api.xiaomimimo.com/v1" || creds["api_key"] != "cfg-key" || !reflect.DeepEqual(creds["request_overrides"], want) {
		t.Fatalf("creds %v", creds)
	}
}

func TestDelegationCredentialResolutionBaseUrlSurvivesRuntimeResolutionFailure(t *testing.T) {
	scriptRuntimeProviderCall(t, nil, errors.New("MIMO_API_KEY not set"))
	creds, err := resolveCreds(t, mockDelegateParent(t, 0), map[string]any{"model": "m", "provider": "mimo", "base_url": "https://api.xiaomimimo.com/v1", "api_key": "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, has := creds["max_output_tokens"]; has || creds["base_url"] != "https://api.xiaomimimo.com/v1" || creds["request_overrides"] != nil {
		t.Fatalf("creds %v", creds)
	}
}

func TestDelegationCredentialResolutionProviderResolutionFailureRaisesValueerror(t *testing.T) {
	scriptRuntimeProviderCall(t, nil, errors.New("OPENROUTER_API_KEY not set"))
	_, err := resolveCreds(t, mockDelegateParent(t, 0), map[string]any{"model": "some-model", "provider": "openrouter"})
	var refusal *tools.DelegationValueError
	if !errors.As(err, &refusal) || !strings.Contains(strings.ToLower(err.Error()), "openrouter") {
		t.Fatalf("err %v", err)
	}
}

func TestDelegationCredentialResolutionProviderResolvesButNoApiKeyRaises(t *testing.T) {
	scriptRuntimeProviderCall(t, &provider.Runtime{Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKey: "", APIMode: "chat_completions"}, nil)
	_, err := resolveCreds(t, mockDelegateParent(t, 0), map[string]any{"model": "some-model", "provider": "openrouter"})
	var refusal *tools.DelegationValueError
	if !errors.As(err, &refusal) {
		t.Fatalf("err %v", err)
	}
}

func TestDelegationCredentialResolutionNamedCustomProviderPreservesProviderName(t *testing.T) {
	calls := scriptRuntimeProviderCall(t, &provider.Runtime{Provider: "custom", Model: "deepseek-v4-pro-CEER", BaseURL: "https://api.crof.ai/v1", APIKey: "crof-key-abc", APIMode: "chat_completions"}, nil)
	creds, err := resolveCreds(t, mockDelegateParent(t, 0), map[string]any{"model": "deepseek-v4-pro-CEER", "provider": "crof.ai"})
	if err != nil {
		t.Fatal(err)
	}
	if creds["provider"] != "crof.ai" || creds["model"] != "deepseek-v4-pro-CEER" || creds["base_url"] != "https://api.crof.ai/v1" || creds["api_key"] != "crof-key-abc" {
		t.Fatalf("creds %v", creds)
	}
	if len(*calls) != 1 || (*calls)[0] != [2]string{"crof.ai", "deepseek-v4-pro-CEER"} {
		t.Fatalf("resolver calls %v", *calls)
	}
}

func TestDelegationProviderIntegrationCrossProviderDelegation(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	writeDelegationBlock(t, parent.env.Home, "  max_iterations: 45\n  model: google/gemini-3-flash-preview\n  provider: openrouter\n")
	prev := resolveDelegationCredentialsFn
	resolveDelegationCredentialsFn = func(context.Context, map[string]any, *AIAgent) (map[string]any, error) {
		return map[string]any{"model": "google/gemini-3-flash-preview", "provider": "openrouter", "base_url": "https://openrouter.ai/api/v1", "api_key": "sk-or-key", "api_mode": "chat_completions"}, nil
	}
	t.Cleanup(func() { resolveDelegationCredentialsFn = prev })
	parent.provider, parent.baseURL, parent.apiKey = "nous", "https://inference-api.nousresearch.com/v1", "nous-key-abc"
	built := captureChildBuilds(t, parent.env, "done")
	if _, err := delegateTask(tools.WithToolEnv(context.Background(), parent.env), parent, map[string]any{"goal": "Cross-provider test"}); err != nil {
		t.Fatal(err)
	}
	if len(*built) != 1 {
		t.Fatalf("built %d", len(*built))
	}
	o := (*built)[0]
	if o.Provider != "openrouter" || o.BaseURL != "https://openrouter.ai/api/v1" || o.APIKey != "sk-or-key" || o.BaseURL == parent.baseURL || o.APIKey == parent.apiKey {
		t.Fatalf("options %+v", o)
	}
}

func TestDelegationProviderIntegrationCredentialErrorReturnsJsonError(t *testing.T) {
	parent := mockDelegateParent(t, 0)
	writeDelegationBlock(t, parent.env.Home, "  model: bad-model\n  provider: nonexistent\n")
	prev := resolveDelegationCredentialsFn
	resolveDelegationCredentialsFn = func(context.Context, map[string]any, *AIAgent) (map[string]any, error) {
		return nil, &tools.DelegationValueError{Message: "Cannot resolve delegation provider 'nonexistent': Unknown provider"}
	}
	t.Cleanup(func() { resolveDelegationCredentialsFn = prev })
	out, err := delegateTask(tools.WithToolEnv(context.Background(), parent.env), parent, map[string]any{"goal": "Should fail"})
	payload := dispatchPayload(t, out, err)
	if !entryHas(payload, "error") || entryHas(payload, "results") {
		t.Fatalf("payload %v", payload)
	}
}
