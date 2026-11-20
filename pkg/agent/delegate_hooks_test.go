package agent

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func delegateParent() *AIAgent {
	return &AIAgent{baseURL: "https://openrouter.ai/api/v1", apiKey: "parent-key", provider: "openrouter", model: "anthropic/claude-sonnet-4", requestOverrides: map[string]any{}}
}

func TestDelegateRouteAgentReadsAndPinsTheAgentRoute(t *testing.T) {
	var parent tools.DelegateRouteAgent = delegateParent()
	if parent.Provider() != "openrouter" || parent.Model() != "anthropic/claude-sonnet-4" || parent.BaseURL() != "https://openrouter.ai/api/v1" || parent.APIKey() != "parent-key" {
		t.Fatalf("route %q %q %q %q", parent.Provider(), parent.Model(), parent.BaseURL(), parent.APIKey())
	}
	parent.SetRequestOverrides(map[string]any{"service_tier": "priority"})
	if parent.RequestOverrides()["service_tier"] != "priority" {
		t.Fatalf("overrides %v", parent.RequestOverrides())
	}
}

func TestTaskServiceTierRunsThroughTheInstalledProviderHooks(t *testing.T) {
	codex := map[string]any{"provider": "openai-codex", "base_url": "https://chatgpt.com/backend-api/codex", "api_key": "codex-key", "model": "gpt-6-astra"}
	out, err := tools.TaskServiceTier("fast", codex, delegateParent())
	if err != nil {
		t.Fatal(err)
	}
	if out["service_tier"] != "priority" || out["request_overrides"].(map[string]any)["service_tier"] != "priority" {
		t.Fatalf("fast on codex %v", out)
	}
	normal, err := tools.TaskServiceTier("normal", codex, &AIAgent{requestOverrides: map[string]any{"service_tier": "priority", "x": 1}})
	if err != nil || normal["service_tier"] != nil {
		t.Fatalf("normal %v %v", normal, err)
	}
	for _, c := range []struct {
		route  map[string]any
		word   string
		needle string
	}{
		{map[string]any{"provider": "custom", "base_url": "http://127.0.0.1:8080/v1", "api_key": "k", "model": "qwen3-local"}, "fast", "not available"},
		{codex, "turbo", "turbo"},
		{codex, "auto", "auto"},
	} {
		if _, err := tools.TaskServiceTier(c.word, c.route, delegateParent()); err == nil || !strings.Contains(err.Error(), c.needle) {
			t.Fatalf("%s on %v: %v", c.word, c.route["model"], err)
		}
	}
}
