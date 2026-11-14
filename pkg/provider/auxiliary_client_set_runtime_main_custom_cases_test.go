package provider

import (
	"context"
	"strings"
	"testing"
)

// Hermetic home with no aggregator credentials and the given config bound to a cleared runtime
func auxResolveAutoCustomHome(t *testing.T, configYAML string) context.Context {
	t.Helper()
	auxIsolateClientCache(t)
	auxUnsetEnv(t, "OPENROUTER_API_KEY", "NOUS_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL")
	return ClearRuntimeMain(auxMainTestHome(t, configYAML))
}

func TestResolveAutoCustomEndToEndConfigLessCustomEndpointRoutesViaGlobal(t *testing.T) {
	ctx := auxResolveAutoCustomHome(t, "model:\n  default: glm-5.1\n  provider: 'custom:ephemeral'\n  base_url: ''\n")
	ctx = SetRuntimeMain(ctx, "custom:ephemeral", "glm-5.1", RuntimeMainOptions{BaseURL: "https://ephemeral.live/v1", APIKey: "sk-live"})
	client, resolved, err := ResolveProviderClient(ctx, "auto", "", ProviderClientOptions{})
	if err != nil || client == nil {
		t.Fatalf("config-less custom endpoint fell through to Step 2 — the #34777 bug is back: (%v, %v)", client, err)
	}
	if resolved != "glm-5.1" || strings.TrimRight(client.BaseURL(), "/") != "https://ephemeral.live/v1" {
		t.Fatalf("resolved %q base %q", resolved, client.BaseURL())
	}
}

func TestResolveAutoCustomEndToEndNamedCustomWithConfigEntryStillRoutes(t *testing.T) {
	ctx := auxResolveAutoCustomHome(t, "model:\n  default: glm-5.1\n  provider: 'custom:openclaw'\n  base_url: ''\ncustom_providers:\n  - name: openclaw\n    base_url: 'https://withcfg.example/v1'\n    model: glm-5.1\n    api_key: cfg-key\n")
	ctx = SetRuntimeMain(ctx, "custom:openclaw", "glm-5.1", RuntimeMainOptions{})
	client, _, err := ResolveProviderClient(ctx, "auto", "", ProviderClientOptions{})
	if err != nil || client == nil {
		t.Fatalf("got (%v, %v)", client, err)
	}
	if strings.TrimRight(client.BaseURL(), "/") != "https://withcfg.example/v1" {
		t.Fatalf("base %q", client.BaseURL())
	}
}

func TestResolveAutoCustomEndToEndNamedCustomAnthropicMessagesKeepsFullNameAndUrl(t *testing.T) {
	proxyBase := "https://acme.palantirfoundry.com/api/v2/llm/proxy/anthropic"
	ctx := auxResolveAutoCustomHome(t, "model:\n  default: claude-4-6-opus\n  provider: 'custom:palantir'\n  base_url: ''\ncustom_providers:\n  - name: palantir\n    base_url: '"+proxyBase+"'\n    model: claude-4-6-opus\n    api_key: foundry-token\n    api_mode: anthropic_messages\n")
	auxFakeAnthropicBuild(t, auxNoAnthropicPool, "")
	ctx = SetRuntimeMain(ctx, "custom:palantir", "claude-4-6-opus", RuntimeMainOptions{BaseURL: proxyBase, APIKey: "foundry-token", APIMode: "anthropic_messages"})
	client, resolved, err := ResolveProviderClient(ctx, "auto", "", ProviderClientOptions{})
	if err != nil || client == nil {
		t.Fatalf("custom:<name> with anthropic_messages entry resolved to None: %v", err)
	}
	ac, ok := client.(*AnthropicAuxiliaryClient)
	if !ok || resolved != "claude-4-6-opus" {
		t.Fatalf("expected AnthropicAuxiliaryClient, got %T resolved %q — the custom:<name> main provider was collapsed to the anonymous-custom OpenAI-wire arm (PR #36043 regression)", client, resolved)
	}
	if strings.TrimRight(ac.BaseURL(), "/") != proxyBase {
		t.Fatalf("base %q", ac.BaseURL())
	}
}
