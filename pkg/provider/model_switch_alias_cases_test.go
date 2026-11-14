package provider

import (
	"testing"
)

func TestOllamaApiKeyUsedForOllamaEndpoint(t *testing.T) {
	ctx, _ := switchAliasHome(t, "model:\n  default: qwen3.5:397b\n  provider: custom\n  base_url: https://ollama.com/v1\n")
	t.Setenv("OLLAMA_API_KEY", "test-ollama-key-12345")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	rt, err := modelsResolver(ctx).ResolveRuntime("custom", "", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if rt.BaseURL != "https://ollama.com/v1" || rt.APIKey != "test-ollama-key-12345" || rt.Provider != "custom" {
		t.Fatalf("runtime %+v", rt)
	}
}

func TestOllamaKeyIsHostMatchedNotSubstringMatched(t *testing.T) {
	ctx, h := switchAliasHome(t, "")
	t.Setenv("OLLAMA_API_KEY", "sk-ollama-KEY")
	for _, tc := range []struct {
		baseURL   string
		expectKey bool
	}{
		{"https://ollama.com/v1", true},
		{"https://ollama.com.attacker.test/v1", false},
		{"http://127.0.0.1/ollama.com/v1", false},
	} {
		rt, err := modelsResolver(ctx).resolveNamedCustomRuntime("custom", "", tc.baseURL, "", readConfigYAMLMap(t, h))
		if err != nil {
			t.Fatalf("%s: %v", tc.baseURL, err)
		}
		if (rt.APIKey == "sk-ollama-KEY") != tc.expectKey {
			t.Fatalf("%s runtime %+v", tc.baseURL, rt)
		}
	}
}
