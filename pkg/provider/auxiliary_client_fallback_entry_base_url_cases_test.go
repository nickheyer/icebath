package provider

import (
	"context"
	"strings"
	"testing"
)

const (
	auxRelayAnthropicBase  = "http://127.0.0.1:9002/anthropic"
	auxRelayOpenrouterBase = "http://127.0.0.1:9002/openrouter/v1"
	auxFallbackEntryKey    = "sk-test-not-a-real-key"
)

// Temp home with every OpenAI, Anthropic and OpenRouter credential variable removed
func auxFallbackEntryHome(t *testing.T) context.Context {
	t.Helper()
	auxIsolateClientCache(t)
	auxUnsetEnv(t, "OPENAI_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN",
		"ANTHROPIC_BASE_URL", "OPENROUTER_API_KEY", "OPENROUTER_BASE_URL")
	return auxMainTestHome(t, "")
}

func TestAnthropicFallbackEntryBaseUrlIsTheResolvedEndpoint(t *testing.T) {
	ctx := auxFallbackEntryHome(t)
	captured := auxFakeAnthropicBuild(t, auxNoAnthropicPool, "")
	client, _, err := ResolveProviderClient(ctx, "anthropic", "claude-haiku-4-5-20251001",
		ProviderClientOptions{RawCodex: true, ExplicitBaseURL: auxRelayAnthropicBase, ExplicitAPIKey: auxFallbackEntryKey})
	if err != nil || client == nil {
		t.Fatalf("got (%v, %v)", client, err)
	}
	if captured[1] != auxRelayAnthropicBase || client.BaseURL() != auxRelayAnthropicBase {
		t.Fatalf("built %v client base %q", captured[1], client.BaseURL())
	}
}

func TestAnthropicFallbackEntryRefusesANonAnthropicEndpoint(t *testing.T) {
	ctx := auxFallbackEntryHome(t)
	captured := auxFakeAnthropicBuild(t, auxNoAnthropicPool, "")
	client, _, err := ResolveProviderClient(ctx, "anthropic", "claude-haiku-4-5-20251001",
		ProviderClientOptions{RawCodex: true, ExplicitBaseURL: "http://127.0.0.1:9002/openai/v1", ExplicitAPIKey: auxFallbackEntryKey})
	if err != nil || client != nil {
		t.Fatalf("an incompatible explicit endpoint must not yield a client, got (%v, %v)", client, err)
	}
	if captured[1] != nil {
		t.Fatalf("no canonical client may be constructed on refusal, built %v", captured[1])
	}
}

func TestOpenrouterFallbackEntryBaseUrlIsTheResolvedEndpoint(t *testing.T) {
	ctx := auxFallbackEntryHome(t)
	client, _, err := ResolveProviderClient(ctx, "openrouter", "some/model",
		ProviderClientOptions{RawCodex: true, ExplicitBaseURL: auxRelayOpenrouterBase, ExplicitAPIKey: auxFallbackEntryKey})
	if err != nil || client == nil {
		t.Fatalf("got (%v, %v)", client, err)
	}
	if base := strings.TrimRight(client.BaseURL(), "/"); base != auxRelayOpenrouterBase {
		t.Fatalf("base %q", base)
	}
}

func TestOpenrouterWithoutAnEntryBaseUrlKeepsTheConfiguredDefault(t *testing.T) {
	ctx := auxFallbackEntryHome(t)
	client, _, err := ResolveProviderClient(ctx, "openrouter", "some/model", ProviderClientOptions{RawCodex: true, ExplicitAPIKey: auxFallbackEntryKey})
	if err != nil || client == nil {
		t.Fatalf("got (%v, %v)", client, err)
	}
	if base := strings.TrimRight(client.BaseURL(), "/"); base != strings.TrimRight(OpenRouterBaseURL, "/") {
		t.Fatalf("base %q", base)
	}
}
