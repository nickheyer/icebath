package provider

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Swaps both config load seams to one fixed tree until the test ends
func auxFakeConfigTree(t *testing.T, tree config.Tree) {
	t.Helper()
	prevLoad, prevReadonly := auxLoadConfigFn, auxLoadConfigReadonlyFn
	auxLoadConfigFn = func(context.Context) (config.Tree, error) { return tree, nil }
	auxLoadConfigReadonlyFn = func(context.Context) (config.Tree, error) { return tree, nil }
	t.Cleanup(func() { auxLoadConfigFn, auxLoadConfigReadonlyFn = prevLoad, prevReadonly })
}

// Resolves the custom provider against an explicit endpoint and returns the key the client carries
func auxCustomEndpointKey(t *testing.T, ctx context.Context, baseURL string, explicitKey any) string {
	t.Helper()
	client, _, err := ResolveProviderClient(ctx, "custom", "test-model", ProviderClientOptions{ExplicitBaseURL: baseURL, ExplicitAPIKey: explicitKey})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("no client resolved")
	}
	return client.APIKey()
}

func TestCustomEndpointApiKeyInheritanceInheritsMainApiKeyWhenAuxKeyEmpty(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	auxFakeConfigTree(t, config.Tree{"model": map[string]any{"api_key": "sk-main-config-key", "base_url": "https://gw.example.com/v1", "default": "main-model"}})
	if got := auxCustomEndpointKey(t, ctx, "https://gw.example.com/v1", nil); got != "sk-main-config-key" {
		t.Fatalf("Custom endpoint with empty api_key should inherit model.api_key from config, got: %q", got)
	}
}

func TestCustomEndpointApiKeyInheritanceExplicitApiKeyTakesPrecedence(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("OPENAI_API_KEY", "")
	auxFakeConfigTree(t, config.Tree{"model": map[string]any{"api_key": "sk-main-config-key"}})
	if got := auxCustomEndpointKey(t, ctx, "https://gw.example.com/v1", "sk-explicit"); got != "sk-explicit" {
		t.Fatalf("got %q", got)
	}
}

func TestCustomEndpointApiKeyInheritanceRuntimeOverrideKeyIsUsed(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("OPENAI_API_KEY", "")
	auxFakeConfigTree(t, config.Tree{"model": map[string]any{}})
	ctx, _ = ScopedRuntimeMain(ctx, map[string]any{"base_url": "https://gw.example.com/v1", "api_key": "sk-runtime-key"})
	if got := auxCustomEndpointKey(t, ctx, "https://gw.example.com/v1", nil); got != "sk-runtime-key" {
		t.Fatalf("got %q", got)
	}
}

func TestCustomEndpointApiKeyInheritanceCrossHostAuxEndpointDoesNotInheritMainKey(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("OPENAI_API_KEY", "")
	auxFakeConfigTree(t, config.Tree{"model": map[string]any{"api_key": "sk-main-config-key", "base_url": "https://gw.example.com/v1"}})
	if got := auxCustomEndpointKey(t, ctx, "https://other-host.example.net/v1", nil); got != "no-key-required" {
		t.Fatalf("got %q", got)
	}
}

func TestCustomEndpointApiKeyInheritanceMainKeyGoesOnlyToItsOwnOrigin(t *testing.T) {
	cases := []struct {
		name, auxBase string
		liveKey       any
		inherits      bool
	}{
		{"same-origin", "https://gw.example.com:443/v2", nil, true},
		{"http-downgrade", "http://gw.example.com/v1", nil, false},
		{"other-port", "https://gw.example.com:8443/v1", nil, false},
		{"live-keyless", "http://127.0.0.1:8080/v1", "", false},
		{"live-key_cmd", "http://127.0.0.1:8080/v1", func() string { return "sk-live-cmd-key" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "model:\n  provider: custom\n  base_url: https://gw.example.com/v1\n  api_key: sk-main-config-key\n  default: main-model\n" +
				fmt.Sprintf("auxiliary:\n  compression:\n    provider: custom\n    base_url: %s\n    model: aux-model\n", tc.auxBase)
			ctx := auxMainTestHome(t, yaml)
			t.Setenv("OPENAI_API_KEY", "")
			t.Setenv("OPENAI_BASE_URL", "")
			auxClearUnhealthy(t)
			if tc.liveKey != nil {
				ctx = SetRuntimeMain(ctx, "custom", "local-model", RuntimeMainOptions{BaseURL: "http://127.0.0.1:8080/v1", APIKey: tc.liveKey})
			}
			client, model, err := GetTextAuxiliaryClient(ctx, "compression", nil)
			if err != nil {
				t.Fatal(err)
			}
			if model != "aux-model" {
				t.Fatalf("model %q", model)
			}
			if client == nil {
				t.Fatal("no client resolved")
			}
			if (client.APIKey() == "sk-main-config-key") != tc.inherits {
				t.Fatalf("key %q inherits %v", client.APIKey(), tc.inherits)
			}
		})
	}
}

func TestCustomEndpointApiKeyInheritanceCachedClientCarriesOnlyTheLiveMainCredential(t *testing.T) {
	cases := []struct {
		name, auxBase              string
		explicitKey, expectedThird any
	}{
		{"explicit-base-url", "https://gw.example.com/v1", nil, "no-key-required"},
		{"runtime-endpoint", "", nil, nil},
		{"explicit-base-url-blank-key", "https://gw.example.com/v1", "   ", "no-key-required"},
		{"runtime-endpoint-blank-key", "", "   ", nil},
		{"runtime-endpoint-explicit-key", "", "sk-explicit-aux", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := auxMainTestHome(t, "")
			t.Setenv("OPENAI_API_KEY", "")
			t.Setenv("OPENAI_BASE_URL", "")
			ShutdownCachedClients(ctx)
			t.Cleanup(func() { ShutdownCachedClients(ctx) })
			var served []any
			for _, liveKey := range []string{"sk-first-session", "sk-second-session", ""} {
				live := SetRuntimeMain(ctx, "custom", "main-model", RuntimeMainOptions{BaseURL: "https://gw.example.com/v1", APIKey: liveKey})
				client, _, err := auxGetCachedClient(live, auxCachedClientRequest{Provider: "custom", Model: "aux-model", BaseURL: tc.auxBase, APIKey: tc.explicitKey})
				if err != nil {
					t.Fatal(err)
				}
				if client == nil {
					served = append(served, nil)
				} else {
					served = append(served, client.APIKey())
				}
			}
			want := []any{"sk-first-session", "sk-second-session", tc.expectedThird}
			if !reflect.DeepEqual(served, want) {
				t.Fatalf("served %v want %v", served, want)
			}
		})
	}
}
