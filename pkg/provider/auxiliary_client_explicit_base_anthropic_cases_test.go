package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	auxExplicitAnthropicBase   = "https://gateway.example.com/proxy/anthropic"
	auxExplicitDualSurfaceBase = "https://api.minimax.io/anthropic"
)

// Temp home with the OpenAI and Anthropic credential variables removed
func auxExplicitBaseHome(t *testing.T) context.Context {
	t.Helper()
	auxIsolateClientCache(t)
	auxUnsetEnv(t, "OPENAI_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN")
	return auxMainTestHome(t, "")
}

func auxNoAnthropicPool(context.Context, string) (bool, *PooledCredential) { return false, nil }

func auxResolveCustomExplicit(t *testing.T, ctx context.Context, model, base, apiMode string) (AuxClient, string) {
	t.Helper()
	client, resolved, err := ResolveProviderClient(ctx, "custom", model, ProviderClientOptions{ExplicitBaseURL: base, ExplicitAPIKey: "k", APIMode: apiMode})
	if err != nil || client == nil {
		t.Fatalf("got (%v, %v)", client, err)
	}
	return client, resolved
}

func TestExplicitBaseAnthropicMessagesKeepsAnthropicPath(t *testing.T) {
	ctx := auxExplicitBaseHome(t)
	captured := auxFakeAnthropicBuild(t, auxNoAnthropicPool, "")
	client, model := auxResolveCustomExplicit(t, ctx, "claude-opus-4-8", auxExplicitAnthropicBase, "anthropic_messages")
	ac, ok := client.(*AnthropicAuxiliaryClient)
	if !ok {
		t.Fatalf("custom endpoint with api_mode=anthropic_messages must return the native Anthropic wrapper, got %T", client)
	}
	if captured[0] != "k" || captured[1] != auxExplicitAnthropicBase {
		t.Fatalf("built with %v", captured)
	}
	if ac.BaseURL() != auxExplicitAnthropicBase || model != "claude-opus-4-8" {
		t.Fatalf("base %q model %q", ac.BaseURL(), model)
	}
}

func TestExplicitBaseAnthropicMessagesOpenaiFallbackUsesV1(t *testing.T) {
	ctx := auxExplicitBaseHome(t)
	prev := auxBuildAnthropicClientFn
	t.Cleanup(func() { auxBuildAnthropicClientFn = prev })
	auxBuildAnthropicClientFn = func(context.Context, any, string, float64, bool) (*anthropicSDKClient, error) {
		return nil, errors.New("anthropic package not installed")
	}
	client, _ := auxResolveCustomExplicit(t, ctx, "claude-opus-4-8", auxExplicitDualSurfaceBase, "anthropic_messages")
	if _, isAnthropic := client.(*AnthropicAuxiliaryClient); isAnthropic {
		t.Fatal("the OpenAI wire client must stand in when the Anthropic SDK is unavailable")
	}
	if !strings.HasSuffix(strings.TrimRight(client.BaseURL(), "/"), "/v1") {
		t.Fatalf("base %q", client.BaseURL())
	}
}

func TestExplicitBaseWithoutAnthropicModePreservesV1Rewrite(t *testing.T) {
	ctx := auxExplicitBaseHome(t)
	client, _ := auxResolveCustomExplicit(t, ctx, "my-model", auxExplicitDualSurfaceBase, "chat_completions")
	if _, isAnthropic := client.(*AnthropicAuxiliaryClient); isAnthropic {
		t.Fatal("chat_completions must not build the Anthropic wrapper")
	}
	base := strings.TrimRight(client.BaseURL(), "/")
	if !strings.HasSuffix(base, "/v1") || strings.Contains(base, "/anthropic") {
		t.Fatalf("base %q", base)
	}
}

func TestExplicitBaseUnknownHostKeepsAnthropicPath(t *testing.T) {
	ctx := auxExplicitBaseHome(t)
	client, _ := auxResolveCustomExplicit(t, ctx, "my-model", auxExplicitAnthropicBase, "chat_completions")
	if _, isAnthropic := client.(*AnthropicAuxiliaryClient); isAnthropic {
		t.Fatal("chat_completions must not build the Anthropic wrapper")
	}
	if base := strings.TrimRight(client.BaseURL(), "/"); !strings.HasSuffix(base, "/proxy/anthropic") {
		t.Fatalf("base %q", base)
	}
}
