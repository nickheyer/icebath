package provider

import (
	"sort"
	"testing"
)

func TestCanonicalApiMode(t *testing.T) {
	t.Run("test_alias_maps_to_canonical", func(t *testing.T) {
		for _, tc := range [][2]string{
			{"openai", "chat_completions"}, {"OpenAI", "chat_completions"}, {" openai ", "chat_completions"},
			{"openai_chat", "chat_completions"}, {"chat-completions", "chat_completions"},
			{"responses", "codex_responses"}, {"openai_responses", "codex_responses"},
			{"anthropic", "anthropic_messages"}, {"messages", "anthropic_messages"}, {"bedrock", "bedrock_converse"},
		} {
			if got := CanonicalAPIMode(tc[0]); got != tc[1] {
				t.Fatalf("%q -> %q, want %q", tc[0], got, tc[1])
			}
		}
	})
	t.Run("test_canonical_names_pass_through", func(t *testing.T) {
		var names []string
		for name := range validAPIModes {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) != 5 {
			t.Fatalf("valid modes %v", names)
		}
		for _, canonical := range names {
			if got := CanonicalAPIMode(canonical); got != canonical {
				t.Fatalf("%q -> %q", canonical, got)
			}
		}
	})
	t.Run("test_unknown_value_passes_through_unchanged", func(t *testing.T) {
		if got := CanonicalAPIMode("weird_thing"); got != "weird_thing" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_every_alias_lands_in_the_valid_set", func(t *testing.T) {
		if len(apiModeAliases) != 13 {
			t.Fatalf("%d aliases", len(apiModeAliases))
		}
		for alias, target := range apiModeAliases {
			if !validAPIModes[target] {
				t.Fatalf("alias %q lands on %q", alias, target)
			}
		}
	})
}

func TestCanonicalAPIModeStripsAndFoldsLikePythonStr(t *testing.T) {
	if got := CanonicalAPIMode("\x1f OPENAI\x1c"); got != "chat_completions" {
		t.Fatalf("got %q", got)
	}
	if got := CanonicalAPIMode("\x1fweird\x1c"); got != "weird" {
		t.Fatalf("got %q", got)
	}
}

func TestNormalizedEntryCanonicalizes(t *testing.T) {
	entry := func(apiMode string) map[string]any {
		return map[string]any{"name": "relay", "api": "https://relay.example.invalid/v1", "api_mode": apiMode}
	}
	t.Run("test_legacy_openai_becomes_chat_completions", func(t *testing.T) {
		if got := NormalizeCustomProviderEntry(entry("openai"), "relay")["api_mode"]; got != "chat_completions" {
			t.Fatalf("api_mode %v", got)
		}
	})
	t.Run("test_canonical_value_unchanged", func(t *testing.T) {
		if got := NormalizeCustomProviderEntry(entry("codex_responses"), "relay")["api_mode"]; got != "codex_responses" {
			t.Fatalf("api_mode %v", got)
		}
	})
	t.Run("test_transport_key_also_canonicalized", func(t *testing.T) {
		e := map[string]any{"name": "relay", "api": "https://relay.example.invalid/v1", "transport": "openai"}
		if got := NormalizeCustomProviderEntry(e, "relay")["api_mode"]; got != "chat_completions" {
			t.Fatalf("api_mode %v", got)
		}
	})
}

func TestRuntimeParseApiMode(t *testing.T) {
	r := &resolver{}
	t.Run("test_legacy_openai_is_valid_chat_completions", func(t *testing.T) {
		if got := r.parseAPIMode("openai"); got != "chat_completions" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_canonical_value_still_valid", func(t *testing.T) {
		if got := r.parseAPIMode("anthropic_messages"); got != "anthropic_messages" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_unknown_value_still_rejected", func(t *testing.T) {
		if got := r.parseAPIMode("bogus"); got != "" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_non_string_still_rejected", func(t *testing.T) {
		if got := r.parseAPIMode(nil); got != "" {
			t.Fatalf("nil -> %q", got)
		}
		if got := r.parseAPIMode(42); got != "" {
			t.Fatalf("42 -> %q", got)
		}
	})
}
