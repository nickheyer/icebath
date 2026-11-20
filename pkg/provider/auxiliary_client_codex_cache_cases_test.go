package provider

import (
	"context"
	"testing"
)

// Effective wire body of the last captured responses request with extra_body folded in
func auxCodexWire(client *auxFakeResponsesClient) map[string]any {
	wire := map[string]any{}
	for k, v := range client.captured {
		wire[k] = v
	}
	if extra, ok := client.captured["extra_body"].(map[string]any); ok {
		for k, v := range extra {
			wire[k] = v
		}
	}
	return wire
}

// Runs one codex adapter create with the given messages and extra body and returns the wire body
func auxCodexWireFor(t *testing.T, baseURL, model string, messages []any, extraBody map[string]any) map[string]any {
	t.Helper()
	adapter, client := newAuxCodexTestAdapter(baseURL, model)
	kwargs := map[string]any{"messages": messages}
	if extraBody != nil {
		kwargs["extra_body"] = extraBody
	}
	if _, err := adapter.create(context.Background(), kwargs); err != nil {
		t.Fatal(err)
	}
	return auxCodexWire(client)
}

func auxSysUserHi() []any {
	return []any{map[string]any{"role": "system", "content": "SYS"}, map[string]any{"role": "user", "content": "hi"}}
}

func TestCodexAdapterPromptCacheKeyExtendedCacheModelsSetPromptCacheRetention(t *testing.T) {
	for _, model := range []string{"gpt-4.1", "gpt-5.1-codex-max", "openai.gpt-5.5-pro"} {
		wire := auxCodexWireFor(t, "https://bedrock-mantle.us-west-2.api.aws/v1", model, auxSysUserHi(), nil)
		if wire["prompt_cache_retention"] != "24h" {
			t.Fatalf("%s: retention %v", model, wire["prompt_cache_retention"])
		}
	}
}

func TestCodexAdapterPromptCacheKeyMetaEndpointIncludesPromptCacheRetention(t *testing.T) {
	wire := auxCodexWireFor(t, "https://api.meta.ai/v1", "muse-spark-1.2", auxSysUserHi(), nil)
	if wire["prompt_cache_retention"] != "24h" {
		t.Fatalf("retention %v", wire["prompt_cache_retention"])
	}
}

func TestCodexAdapterPromptCacheKeyPromptCacheRetentionSkippedForCodexBackend(t *testing.T) {
	wire := auxCodexWireFor(t, "https://chatgpt.com/backend-api/codex", "gpt-5.5", auxSysUserHi(), nil)
	if _, ok := wire["prompt_cache_retention"]; ok {
		t.Fatalf("retention sent to the codex backend %v", wire)
	}
}

func TestCodexAdapterPromptCacheKeyAstraAuxiliaryRequestUsesOfficialContract(t *testing.T) {
	wire := auxCodexWireFor(t, "https://api.openai.com/v1", "gpt-6-astra", auxUserHi(), map[string]any{"reasoning": map[string]any{"effort": "none"}})
	reasoning, _ := wire["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" {
		t.Fatalf("astra effort %v", wire["reasoning"])
	}
	if _, ok := wire["prompt_cache_retention"]; ok {
		t.Fatalf("retention sent for astra %v", wire)
	}
}

func TestCodexAdapterPromptCacheKeyAstraAuxiliaryProxyKeepsLegacyEffortContract(t *testing.T) {
	wire := auxCodexWireFor(t, "https://responses.example.com/v1", "gpt-6-astra", auxUserHi(), map[string]any{"reasoning": map[string]any{"effort": "none"}})
	reasoning, _ := wire["reasoning"].(map[string]any)
	if reasoning["effort"] != "none" {
		t.Fatalf("proxy effort %v", wire["reasoning"])
	}
}

func TestCodexAdapterPromptCacheKeyCodexBackendForwardsAuxiliaryServiceTier(t *testing.T) {
	wire := auxCodexWireFor(t, "https://chatgpt.com/backend-api/codex", "gpt-5.6-luna", auxUserHi(), map[string]any{"service_tier": "priority"})
	if wire["service_tier"] != "priority" {
		t.Fatalf("service tier %v", wire["service_tier"])
	}
}

func TestCodexAdapterPromptCacheKeyXaiBackendDropsAuxiliaryServiceTier(t *testing.T) {
	wire := auxCodexWireFor(t, "https://api.x.ai/v1", "grok-4.6", auxUserHi(), map[string]any{"service_tier": "priority"})
	if _, ok := wire["service_tier"]; ok {
		t.Fatalf("service tier reached xai %v", wire)
	}
}

func TestCodexAdapterPromptCacheKeyPromptCacheRetentionSkippedForOtherCompatibleEndpoints(t *testing.T) {
	for _, base := range []string{"https://api.openai.com/v1", "https://example.services.ai.azure.com/openai/v1", "https://responses.example.com/v1"} {
		wire := auxCodexWireFor(t, base, "gpt-5.5", auxSysUserHi(), nil)
		if _, ok := wire["prompt_cache_retention"]; ok {
			t.Fatalf("%s: retention sent %v", base, wire)
		}
	}
}

func TestCodexAdapterPromptCacheKeyPromptCacheRetentionSkippedForXaiAndGithubHosts(t *testing.T) {
	for _, base := range []string{"https://api.x.ai/v1", "https://api.githubcopilot.com"} {
		wire := auxCodexWireFor(t, base, "gpt-5.5", auxSysUserHi(), nil)
		if _, ok := wire["prompt_cache_retention"]; ok {
			t.Fatalf("%s: retention sent %v", base, wire)
		}
	}
}
