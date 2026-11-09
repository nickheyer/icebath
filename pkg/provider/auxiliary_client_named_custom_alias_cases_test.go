package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

const auxBeansCustomProviderYAML = "custom_providers:\n  - name: beans\n    base_url: http://beans.local/v1\n    api_key: k\n"

func TestResolveProviderClientMainAliasMainResolvesToNamedCustomProvider(t *testing.T) {
	ctx := auxMainTestHome(t, "model:\n  default: my-model\n  provider: beans\n"+auxBeansCustomProviderYAML)
	client, model, err := ResolveProviderClient(ctx, "main", "override-model", ProviderClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || model != "override-model" || !strings.Contains(client.BaseURL(), "beans.local") {
		t.Fatalf("client %v model %q", client, model)
	}
}

func TestResolveProviderClientMainAliasMainWithCustomColonPrefix(t *testing.T) {
	ctx := auxMainTestHome(t, "model:\n  default: my-model\n  provider: custom:beans\n"+auxBeansCustomProviderYAML)
	client, _, err := ResolveProviderClient(ctx, "main", "test", ProviderClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || !strings.Contains(client.BaseURL(), "beans.local") {
		t.Fatalf("client %v", client)
	}
}

func TestResolveProviderClientMainAliasMainResolvesPluginProviderAlias(t *testing.T) {
	RegisterProvider(Profile{Name: "acme-hub", Aliases: []string{"acme-hub-alias"}, KeyEnv: []string{"ACME_HUB_API_KEY"}, BaseURL: "https://api.acme-hub.test/v1"})
	t.Cleanup(func() { UnregisterProvider("acme-hub") })
	ctx := auxMainTestHome(t, "model:\n  default: gpt-5.4\n  provider: acme-hub-alias\n")
	prev := auxResolveAPIKeyProviderCredentialsFn
	auxResolveAPIKeyProviderCredentialsFn = func(context.Context, string) (apiKeyCreds, error) {
		return apiKeyCreds{APIKey: "acme_test_token", BaseURL: "https://api.acme-hub.test/v1"}, nil
	}
	t.Cleanup(func() { auxResolveAPIKeyProviderCredentialsFn = prev })
	client, model, err := ResolveProviderClient(ctx, "main", "gpt-5.4", ProviderClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || model != "gpt-5.4" {
		t.Fatalf("client %v model %q", client, model)
	}
	if !strings.Contains(client.BaseURL(), "api.acme-hub.test") || client.APIKey() != "acme_test_token" {
		t.Fatalf("base %q key %q", client.BaseURL(), client.APIKey())
	}
}

func TestResolveProviderClientNamedCustomNamedCustomProvider(t *testing.T) {
	ctx := auxMainTestHome(t, "model:\n  default: test-model\n"+auxBeansCustomProviderYAML)
	client, model, err := ResolveProviderClient(ctx, "beans", "my-model", ProviderClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || model != "my-model" || !strings.Contains(client.BaseURL(), "beans.local") {
		t.Fatalf("client %v model %q", client, model)
	}
}

func TestResolveProviderClientNamedCustomProvidersDictUsesDurablePoolWhenNoInlineKey(t *testing.T) {
	ctx := auxMainTestHome(t, "providers:\n  b-ai:\n    name: B.AI\n    base_url: https://api.b.ai/v1\n")
	auth, _ := json.Marshal(map[string]any{"version": 1, "providers": map[string]any{}, "credential_pool": map[string]any{"b-ai": []any{map[string]any{
		"id": "k1", "label": "primary", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-real-b-ai-pool-key-12345"}}}})
	if err := os.WriteFile(filepath.Join(home.Dir(ctx), "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	client, _, err := ResolveProviderClient(ctx, "b-ai", "b-ai-model", ProviderClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || !strings.Contains(client.BaseURL(), "api.b.ai") {
		t.Fatalf("client %v", client)
	}
	if client.APIKey() != "sk-real-b-ai-pool-key-12345" {
		t.Fatalf("key %q", client.APIKey())
	}
}
