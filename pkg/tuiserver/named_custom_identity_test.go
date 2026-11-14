package tuiserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const sharedRelayURL = "https://relay.example/v1"

// Two named custom entries sharing one endpoint and model, alpha the configured default
func namedCustomConfig() map[string]any {
	return map[string]any{
		"model": map[string]any{"default": "alpha-model", "provider": "custom:alpha"},
		"custom_providers": []any{
			map[string]any{"name": "alpha", "base_url": sharedRelayURL, "api_key": "sk-alpha", "model": "alpha-model"},
			map[string]any{"name": "beta", "base_url": sharedRelayURL, "api_key": "sk-beta", "model": "beta-model"},
		},
	}
}

// Writes a config document as the profile's config.yaml, JSON being YAML
func writeIdentityConfig(t *testing.T, h string, cfg map[string]any) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Agent on the shared relay as the named entry it was requested through
func identityAgent(t *testing.T, model, providerName, requested, baseURL string) *agent.AIAgent {
	t.Helper()
	return testAgent(t, agent.AIAgentOptions{Model: model, Provider: providerName, RequestedProvider: requested, BaseURL: baseURL,
		APIMode: "chat_completions", APIKey: "sk-test"})
}

func TestTUIRuntimeConfigAndSessionInfoKeepRequestedCustomSlug(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeIdentityConfig(t, h, namedCustomConfig())
	ctx := context.Background()
	a := identityAgent(t, "beta-model", "custom", "custom:beta", sharedRelayURL)
	cfg := runtimeModelConfig(ctx, a, nil)
	if cfg["provider"] != "custom:beta" {
		t.Fatalf("runtime config provider %v", cfg["provider"])
	}
	info := sessionInfo(ctx, a, tuigateway.NewSession(map[string]any{"agent": a, "session_key": "s1", "profile_home": h}))
	if info["provider"] != "custom:beta" {
		t.Fatalf("session info provider %v", info["provider"])
	}
	if raw, _ := json.Marshal(cfg); strings.Contains(string(raw), "sk-beta") {
		t.Fatalf("runtime config carries the key: %s", raw)
	}
}

func TestTUINonCustomAdapterKeepsAdapterIdentity(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeIdentityConfig(t, h, namedCustomConfig())
	a := identityAgent(t, "beta-model", "anthropic", "custom:beta", sharedRelayURL)
	if got := runtimeModelConfig(context.Background(), a, nil)["provider"]; got != "anthropic" {
		t.Fatalf("provider %v", got)
	}
}

func TestTUIRuntimeConfigDoesNotPairAnEditedEntryKeyWithTheOldEndpoint(t *testing.T) {
	const oldURL, newURL = "https://old.example/v1", "https://new.example/v1"
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	cfg := map[string]any{
		"model": map[string]any{"default": "m", "provider": "custom:alpha"},
		"custom_providers": []any{
			map[string]any{"name": "alpha", "base_url": oldURL, "api_key": "sk-old", "model": "m"},
			map[string]any{"name": "old-owner", "base_url": oldURL, "api_key": "sk-old", "model": "m"},
		},
	}
	writeIdentityConfig(t, h, cfg)
	rt, err := provider.ResolveRuntimeProvider(ctx, h, "custom:alpha", "m", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a := identityAgent(t, "m", rt.Provider, rt.RequestedProvider, rt.BaseURL)
	entry := cfg["custom_providers"].([]any)[0].(map[string]any)
	entry["base_url"], entry["api_key"] = newURL, "sk-new"
	writeIdentityConfig(t, h, cfg)
	modelConfig, err := json.Marshal(runtimeModelConfig(ctx, a, nil))
	if err != nil {
		t.Fatal(err)
	}
	row := pyjson.ObjectOf("model", "m", "model_config", string(modelConfig))
	overrides := storedSessionRuntimeOverrides(ctx, row)
	providerOverride, _ := overrides["provider_override"].(string)
	_, restored, _, err := resolveAgentModelRuntime(ctx, overrides["model_override"], providerOverride)
	if err != nil {
		t.Fatal(err)
	}
	if restored.BaseURL != oldURL || restored.APIKey != "sk-old" {
		t.Fatalf("resumed route %s %s", restored.BaseURL, restored.APIKey)
	}
}
