package provider

import (
	"os"
	"strings"
	"testing"
)

var auxSessionRuntime = map[string]any{"provider": "openai-api", "model": "gpt-5.4",
	"base_url": "https://proxy.example:8443/v1", "api_key": "sk-session"}

// Removes environment keys for the test the way monkeypatch.delenv does
func auxUnsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenaiAliasPrefersConfiguredEndpointOverPublicDefault(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	t.Setenv("OPENAI_BASE_URL", "https://llm-proxy.corp.example/v1")
	provider, base := ExpandDirectAPIAlias(ctx, "openai", "")
	if provider != "custom" || base != "https://llm-proxy.corp.example/v1" {
		t.Fatalf("got (%q, %q)", provider, base)
	}
	auxUnsetEnv(t, "OPENAI_BASE_URL")
	if provider, base = ExpandDirectAPIAlias(ctx, "openai", ""); provider != "custom" || base != "https://api.openai.com/v1" {
		t.Fatalf("got (%q, %q)", provider, base)
	}
}

func TestSessionKeyRejectedAtForeignOriginIsNotRotated(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	for _, rejectingBase := range []string{"https://api.openai.com/v1/", "https://proxy.example:9443/v1/", "http://proxy.example:8443/v1/"} {
		provider, err := auxRecoverablePoolProvider(ctx, "openai-api", auxBareClient{base: rejectingBase, key: "sk-session"}, auxSessionRuntime)
		if err != nil || provider != "" {
			t.Fatalf("%s: got (%q, %v)", rejectingBase, provider, err)
		}
	}
}

func TestRotationSurvivesAtSessionOriginAndForIndependentAuxPool(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	sameOrigin := auxBareClient{base: "https://proxy.example:8443/v1/", key: "sk-session"}
	if provider, err := auxRecoverablePoolProvider(ctx, "openai-api", sameOrigin, auxSessionRuntime); err != nil || provider != "openai-api" {
		t.Fatalf("same origin: got (%q, %v)", provider, err)
	}
	independent := auxBareClient{base: "https://api.openai.com/v1/", key: "sk-aux-pool-1"}
	if provider, err := auxRecoverablePoolProvider(ctx, "openai-api", independent, auxSessionRuntime); err != nil || provider != "openai-api" {
		t.Fatalf("independent pool: got (%q, %v)", provider, err)
	}
}

func TestNamedProviderDefaultsComposeUnderTaskOverrides(t *testing.T) {
	auxIsolateClientCache(t)
	t.Setenv("NAMED_KEY", "named-key")
	ctx := auxMainTestHome(t, "model:\n  provider: openai\n  default: gpt-5.4\nproviders:\n  openai:\n    api: https://named.example/v1\n    key_env: NAMED_KEY\n")
	route := func(explicitBase string, explicitKey any) [2]string {
		t.Helper()
		auxClientCacheMu.Lock()
		clear(auxClientCache)
		auxClientCacheOrder = nil
		auxClientCacheMu.Unlock()
		client, _, err := ResolveProviderClient(ctx, "openai", "gpt-5.4-mini", ProviderClientOptions{ExplicitBaseURL: explicitBase, ExplicitAPIKey: explicitKey})
		if err != nil || client == nil {
			t.Fatalf("got (%v, %v)", client, err)
		}
		return [2]string{strings.TrimRight(client.BaseURL(), "/"), client.APIKey()}
	}
	if got := route("", nil); got != [2]string{"https://named.example/v1", "named-key"} {
		t.Fatalf("defaults %v", got)
	}
	if got := route("https://aux-explicit.example/v1", nil); got != [2]string{"https://aux-explicit.example/v1", "named-key"} {
		t.Fatalf("url override %v", got)
	}
	if got := route("", "task-key"); got != [2]string{"https://named.example/v1", "task-key"} {
		t.Fatalf("key override %v", got)
	}
	if got := route("https://aux-explicit.example/v1", "task-key"); got != [2]string{"https://aux-explicit.example/v1", "task-key"} {
		t.Fatalf("both overrides %v", got)
	}
}
