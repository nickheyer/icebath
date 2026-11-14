package provider

import "testing"

func TestPromptCacheMetaHostMandatedResponses(t *testing.T) {
	for _, u := range []string{"https://api.meta.ai/v1", "https://api.meta.ai/v1/", "https://api.meta.ai/v1/chat/completions", "https://API.META.AI/v1",
		"https://api.meta.ai", "https://api.meta.ai:443/v1", "https://api.meta.ai./v1", "https://attacker.test@api.meta.ai/v1"} {
		if got := HostMandatedMode(u); got != ModeCodexResponses {
			t.Fatalf("%s mandated %q", u, got)
		}
	}
	none := map[string]bool{"https://generic.example.com/v1": true, "https://[::1]/v1": true, "": true, "https://meta.ai/v1": true,
		"https://api.meta.ai.attacker.test/v1": true, "https://proxy.test/api.meta.ai/v1": true}
	for _, u := range []string{"https://api.meta.ai.attacker.test/v1", "https://proxy.test/api.meta.ai/v1", "https://api.meta.ai.evil/v1", "https://meta.ai/v1",
		"https://www.meta.ai/v1", "https://api.meta.com/v1", "https://[::1]/v1", "https://generic.example.com/v1", ""} {
		got := HostMandatedMode(u)
		if got == ModeCodexResponses || (none[u] && got != "") {
			t.Fatalf("spoof %q mandated %q", u, got)
		}
	}
	if DetermineMode("meta", "https://api.meta.ai/v1", "", "") != ModeCodexResponses || DetermineMode("custom", "https://api.meta.ai/v1", "", "") != ModeCodexResponses {
		t.Fatal("meta route must determine codex_responses")
	}
	if DetermineMode("generic", "https://generic.example.com/v1", "", "") != ModeChatCompletions {
		t.Fatal("generic route must determine chat_completions")
	}
	if DetermineMode("meta", "https://api.meta.ai/v1/", "", "") != ModeCodexResponses {
		t.Fatal("trailing slash lost the meta mode")
	}
	for _, u := range []string{"https://api.meta.ai/v1", "https://api.meta.ai/v1/chat/completions", "https://API.META.AI/v1"} {
		if detectAPIModeForURL(u) != ModeCodexResponses {
			t.Fatalf("detect %s", u)
		}
	}
	for _, u := range []string{"https://api.meta.ai.attacker.test/v1", "https://proxy.test/api.meta.ai/v1", "https://meta.ai/v1", "https://generic.example.com/v1"} {
		if got := detectAPIModeForURL(u); got != "" {
			t.Fatalf("detect spoof %s gave %q", u, got)
		}
	}
	r := &resolver{cfg: map[string]any{}}
	if r.fallbackAPIMode("meta", "https://api.meta.ai/v1", "muse-spark-1.2") != ModeCodexResponses || r.fallbackAPIMode("custom", "https://api.meta.ai/v1", "muse-spark-1.2") != ModeCodexResponses {
		t.Fatal("fallback mode for meta must be codex_responses")
	}
	if r.fallbackAPIMode("custom", "https://generic.example.com/v1", "muse-spark-1.2") != ModeChatCompletions {
		t.Fatal("generic fallback must stay chat_completions")
	}
}

func TestPromptCacheMetaConfigRoundtrip(t *testing.T) {
	normalized := NormalizeCustomProviderEntry(map[string]any{"name": "Meta", "base_url": "https://api.meta.ai/v1", "api_mode": "codex_responses"}, "")
	if normalized["api_mode"] != "codex_responses" {
		t.Fatalf("api_mode %#v", normalized)
	}
	normalized = NormalizeCustomProviderEntry(map[string]any{"name": "Meta", "base_url": "https://api.meta.ai/v1", "transport": "codex_responses"}, "")
	if normalized["api_mode"] != "codex_responses" && normalized["transport"] != "codex_responses" {
		t.Fatalf("transport lost %#v", normalized)
	}
}
