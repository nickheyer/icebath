package provider

import (
	"reflect"
	"testing"
)

var astraSpellings = []string{"gpt-6-astra", "openai/gpt-6-astra", "gpt-6-astra-900k", "openai/gpt-6-astra-900k"}

var solSpellings = []string{"gpt-6.1-sol", "openai/gpt-6.1-sol", "gpt-6.1-sol-900k", "openai/gpt-6.1-sol-900k"}

func TestUltrafastIsRequestedOnlyForUltrafastModelsOnFirstPartyRoutes(t *testing.T) {
	routes := [][2]string{{"openai", "https://api.openai.com/v1"}, {"openai-api", "https://api.openai.com/v1"}, {"openai-codex", "https://chatgpt.com/backend-api/codex"}}
	for _, route := range routes {
		providerName, baseURL := route[0], route[1]
		for _, model := range append(append([]string{}, astraSpellings...), solSpellings...) {
			if !ModelSupportsUltrafast(model) {
				t.Fatalf("%s must support ultrafast", model)
			}
			got := ResolveFastModeOverrides(model, providerName, baseURL, "ultrafast")
			if !reflect.DeepEqual(got, map[string]any{"service_tier": "ultrafast"}) {
				t.Fatalf("%s on %s: got %v", model, providerName, got)
			}
		}
		if got := ResolveFastModeOverrides("gpt-6-sol", providerName, baseURL, ""); !reflect.DeepEqual(got, map[string]any{"service_tier": "priority"}) {
			t.Fatalf("gpt-6-sol priority: got %v", got)
		}
		for _, model := range []string{"gpt-6-sol", "gpt-6.1-sol-pro", "gpt-6.1-sol-mini", "gpt-6.1-sol-codex", "gpt-6.1-sol-preview"} {
			if ModelSupportsUltrafast(model) {
				t.Fatalf("%s must not support ultrafast", model)
			}
			if got := ResolveFastModeOverrides(model, providerName, baseURL, "ultrafast"); got != nil {
				t.Fatalf("%s ultrafast must be nil, got %v", model, got)
			}
		}
		for _, model := range append(append([]string{}, astraSpellings...), solSpellings...) {
			if got := ResolveFastModeOverrides(model, "openrouter", "https://openrouter.ai/api/v1", "ultrafast"); got != nil {
				t.Fatalf("proxy must not see the tier, got %v", got)
			}
			if got := ResolveFastModeOverrides(model, "openai-api", "https://proxy.example/v1", "ultrafast"); got != nil {
				t.Fatalf("foreign host under a first party name must not see the tier, got %v", got)
			}
		}
	}
}

func TestResolveFastModeOverridesAnthropicAndGrok(t *testing.T) {
	if got := ResolveFastModeOverrides("claude-opus-4-8", "anthropic", "https://api.anthropic.com", ""); !reflect.DeepEqual(got, map[string]any{"speed": "fast"}) {
		t.Fatalf("anthropic fast mode: got %v", got)
	}
	if got := ResolveFastModeOverrides("anthropic/claude-opus-5.5", "", "", ""); !reflect.DeepEqual(got, map[string]any{"speed": "fast"}) {
		t.Fatalf("unrouted anthropic fast mode: got %v", got)
	}
	if got := ResolveFastModeOverrides("claude-opus-4-8", "openrouter", "https://openrouter.ai/api/v1", ""); got != nil {
		t.Fatalf("anthropic model through a proxy: got %v", got)
	}
	if got := ResolveFastModeOverrides("claude-opus-4-7", "anthropic", "https://api.anthropic.com", ""); got != nil {
		t.Fatalf("opus 4.7 rejects the speed param: got %v", got)
	}
	if got := ResolveFastModeOverrides("x-ai/grok-4.6", "xai", "https://api.x.ai/v1", ""); !reflect.DeepEqual(got, map[string]any{"service_tier": "priority"}) {
		t.Fatalf("grok priority: got %v", got)
	}
	if got := ResolveFastModeOverrides("gpt-5.4-codex", "openai", "https://api.openai.com/v1", ""); got != nil {
		t.Fatalf("codex series is excluded: got %v", got)
	}
	if got := ResolveFastModeOverrides("o3-pro", "openai", "", ""); !reflect.DeepEqual(got, map[string]any{"service_tier": "priority"}) {
		t.Fatalf("o series priority: got %v", got)
	}
}
