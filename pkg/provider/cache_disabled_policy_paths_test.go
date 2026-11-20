package provider

import (
	"context"
	"testing"
)

func TestSharedPredicateMatchesAgentInitSemantics(t *testing.T) {
	for _, ttl := range []any{false, nil, "off", "false", "disabled", "no", "none", "OFF"} {
		if !CacheTTLMeansDisabled(ttl) {
			t.Fatalf("%v must disable caching", ttl)
		}
	}
	for _, ttl := range []any{"5m", "1h", "2h", 5, true, "weird"} {
		if CacheTTLMeansDisabled(ttl) {
			t.Fatalf("%v must keep caching", ttl)
		}
	}
}

// System prompt and one user turn on the native Anthropic destination
func nativePlanMessages() []map[string]any {
	return []map[string]any{{"role": "system", "content": "You are helpful."}, {"role": "user", "content": "hello"}}
}

func TestExplicitCacheDisabledStripsMarkers(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "web_search", "description": "search",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}
	disabled := true
	msgs, outTools := PlanCacheSectionsForDestination(context.Background(), nativePlanMessages(), tools, "anthropic",
		"https://api.anthropic.com", "anthropic_messages", "claude-opus-4.8", &disabled, "", nil)
	if len(collectCacheControls(msgs)) > 0 || len(collectCacheControls(outTools)) > 0 {
		t.Fatalf("markers survived an explicit disable: %v %v", msgs, outTools)
	}
}

func TestConfigOffWithoutExplicitFlag(t *testing.T) {
	cacheTestHome(t, "off")
	msgs, outTools := PlanCacheSectionsForDestination(context.Background(), nativePlanMessages(), nil, "anthropic",
		"https://api.anthropic.com", "anthropic_messages", "claude-opus-4.8", nil, "", nil)
	if len(collectCacheControls(msgs)) > 0 {
		t.Fatalf("config off still marked messages: %v", msgs)
	}
	if outTools != nil && len(collectCacheControls(outTools)) > 0 {
		t.Fatalf("config off still marked tools: %v", outTools)
	}
}

func TestEnabledStillAddsMarkersOnNativeAnthropic(t *testing.T) {
	cacheTestHome(t, "5m")
	messages := append(nativePlanMessages(), map[string]any{"role": "assistant", "content": "hi"}, map[string]any{"role": "user", "content": "again"})
	enabled := false
	msgs, _ := PlanCacheSectionsForDestination(context.Background(), messages, nil, "anthropic",
		"https://api.anthropic.com", "anthropic_messages", "claude-opus-4.8", &enabled, "", nil)
	if len(collectCacheControls(msgs)) == 0 {
		t.Fatal("With caching enabled, native Anthropic destinations must still receive cache_control breakpoints.")
	}
}
