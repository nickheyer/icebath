package provider

import (
	"context"
	"errors"
	"testing"
)

func TestACustomEndpointCarriesOnlyItsOwnEntrysHeaders(t *testing.T) {
	for _, tc := range []struct{ baseURL, sent string }{
		{"https://gw.example.com/v1", "tok"}, {"https://GW.example.com/v1/", "tok"},
		{"https://other.example.com/v1", ""}, {"https://gw.example.com.attacker.test/v1", ""}, {"https://gw.example.com/v1/extra", ""},
	} {
		auxIsolateClientCache(t)
		auxUnsetEnv(t, "OPENAI_API_KEY")
		ctx := auxMainTestHome(t, "custom_providers:\n  - name: routed\n    base_url: https://gw.example.com/v1\n    extra_headers:\n      X-Route-Token: tok\n")
		client, _, err := ResolveProviderClient(ctx, "custom", "m", ProviderClientOptions{ExplicitBaseURL: tc.baseURL, ExplicitAPIKey: "sk-x"})
		if err != nil || client == nil {
			t.Fatalf("%s: no client was built (%v, %v)", tc.baseURL, client, err)
		}
		oc, ok := client.(*auxOpenAIClient)
		if !ok {
			t.Fatalf("%s: want the OpenAI wire client, got %T", tc.baseURL, client)
		}
		if got := oc.customHeaders["X-Route-Token"]; got != tc.sent {
			t.Fatalf("%s: X-Route-Token %q, want %q", tc.baseURL, got, tc.sent)
		}
	}
}

func TestModelParticipatesInClientCacheKey(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	keyFor := func(model string) auxClientCacheKey {
		key, _, err := auxClientCacheKeyFor(ctx, "openrouter", false, "https://openrouter.ai/api/v1", "K", "", nil, false, "", model)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	opus, gpt := keyFor("anthropic/claude-opus-4.8"), keyFor("openai/gpt-5.5")
	if opus == gpt {
		t.Fatalf("different models shared the key %+v", opus)
	}
	if keyFor("anthropic/claude-opus-4.8") != opus {
		t.Fatal("the same model must reuse its key")
	}
}

func TestCustomEndpointAnthropicMessagesFallsBackWhenSdkMissing(t *testing.T) {
	auxUnsetEnv(t, "OPENAI_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN")
	ctx := auxMainTestHome(t, "")
	auxFakeCustomRuntime(t, "https://api.minimax.io/anthropic", "k", "anthropic_messages", "claude-sonnet-4-6")
	prev := auxBuildAnthropicClientFn
	t.Cleanup(func() { auxBuildAnthropicClientFn = prev })
	auxBuildAnthropicClientFn = func(context.Context, any, string, float64, bool) (*anthropicSDKClient, error) {
		return nil, errors.New("anthropic package not installed")
	}
	client, model, err := auxTryCustomEndpoint(ctx)
	if err != nil || client == nil || model != "claude-sonnet-4-6" {
		t.Fatalf("got (%v, %q, %v)", client, model, err)
	}
	if _, isAnthropic := client.(*AnthropicAuxiliaryClient); isAnthropic {
		t.Fatal("the OpenAI wire client must stand in when the Anthropic SDK is unavailable")
	}
}
