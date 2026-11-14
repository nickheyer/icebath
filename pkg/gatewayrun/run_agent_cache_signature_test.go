package gatewayrun

import "testing"

// test_agent_cache.py TestAgentConfigSignature test_model_change_different_signature
func TestModelChangeDifferentSignature(t *testing.T) {
	runtime := map[string]any{"api_key": "sk-test12345678", "base_url": "https://openrouter.ai/api/v1", "provider": "openrouter"}
	sig1 := agentConfigSignature("claude-sonnet-4", runtime, []string{"hermes-telegram"}, "", nil, "", "", false)
	sig2 := agentConfigSignature("claude-opus-4.6", runtime, []string{"hermes-telegram"}, "", nil, "", "", false)
	if sig1 == sig2 {
		t.Fatal("model change kept the signature")
	}
}

// test_agent_cache.py TestAgentConfigSignature test_same_token_prefix_different_full_token_changes_signature
func TestSameTokenPrefixDifferentFullTokenChangesSignature(t *testing.T) {
	rt1 := map[string]any{"api_key": "eyJhbGci.token-for-account-a", "base_url": "https://chatgpt.com/backend-api/codex",
		"provider": "openai-codex", "api_mode": "codex_responses"}
	rt2 := map[string]any{"api_key": "eyJhbGci.token-for-account-b", "base_url": "https://chatgpt.com/backend-api/codex",
		"provider": "openai-codex", "api_mode": "codex_responses"}
	if rt1["api_key"].(string)[:8] != rt2["api_key"].(string)[:8] {
		t.Fatal("fixtures must share a prefix")
	}
	sig1 := agentConfigSignature("gpt-5.3-codex", rt1, []string{"hermes-telegram"}, "", nil, "", "", false)
	sig2 := agentConfigSignature("gpt-5.3-codex", rt2, []string{"hermes-telegram"}, "", nil, "", "", false)
	if sig1 == sig2 {
		t.Fatal("tokens sharing a prefix collided")
	}
}

// test_agent_cache.py TestAgentConfigSignature test_provider_change_different_signature
func TestProviderChangeDifferentSignature(t *testing.T) {
	rt1 := map[string]any{"api_key": "sk-test12345678", "base_url": "https://openrouter.ai/api/v1", "provider": "openrouter"}
	rt2 := map[string]any{"api_key": "sk-test12345678", "base_url": "https://api.anthropic.com", "provider": "anthropic"}
	sig1 := agentConfigSignature("claude-sonnet-4", rt1, []string{"hermes-telegram"}, "", nil, "", "", false)
	sig2 := agentConfigSignature("claude-sonnet-4", rt2, []string{"hermes-telegram"}, "", nil, "", "", false)
	if sig1 == sig2 {
		t.Fatal("provider change kept the signature")
	}
}

// test_agent_cache.py TestAgentConfigSignature test_capability_change_different_signature
func TestCapabilityChangeDifferentSignature(t *testing.T) {
	native := map[string]any{"api_key": "sk-test12345678", "base_url": "https://proxy.example/v1", "provider": "custom",
		"capabilities": map[string]any{"openai_native_compaction": true}}
	plain := map[string]any{"api_key": "sk-test12345678", "base_url": "https://proxy.example/v1", "provider": "custom",
		"capabilities": map[string]any{"openai_native_compaction": false}}
	if agentConfigSignature("gpt-5.6", native, nil, "", nil, "", "", false) == agentConfigSignature("gpt-5.6", plain, nil, "", nil, "", "", false) {
		t.Fatal("capability change kept the signature")
	}
}

// test_agent_cache.py TestAgentConfigSignature test_compression_threshold_change_busts_cache
func TestCompressionThresholdChangeBustsCache(t *testing.T) {
	runtime := map[string]any{"api_key": "k", "base_url": "u", "provider": "p"}
	sig1 := agentConfigSignature("m", runtime, nil, "", map[string]any{"compression.threshold": 0.50}, "", "", false)
	sig2 := agentConfigSignature("m", runtime, nil, "", map[string]any{"compression.threshold": 0.75}, "", "", false)
	if sig1 == sig2 {
		t.Fatal("threshold change kept the signature")
	}
}

// test_agent_cache.py TestAgentConfigSignature test_cache_keys_key_order_does_not_matter
func TestCacheKeysKeyOrderDoesNotMatter(t *testing.T) {
	runtime := map[string]any{"api_key": "k", "base_url": "u", "provider": "p"}
	sigA := agentConfigSignature("m", runtime, nil, "", map[string]any{"model.context_length": 200_000, "compression.threshold": 0.5}, "", "", false)
	sigB := agentConfigSignature("m", runtime, nil, "", map[string]any{"compression.threshold": 0.5, "model.context_length": 200_000}, "", "", false)
	if sigA != sigB {
		t.Fatal("key order changed the signature")
	}
}
