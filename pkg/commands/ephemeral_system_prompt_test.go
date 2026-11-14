package commands

import (
	"strings"
	"testing"
)

func ephemeralPromptConfig(personality string, personalities map[string]any) map[string]any {
	return map[string]any{
		"display": map[string]any{"personality": personality},
		"agent":   map[string]any{"system_prompt": "manual forever", "personalities": personalities},
	}
}

func TestResolveUsesNamedPersonalityWhenSet(t *testing.T) {
	cfg := ephemeralPromptConfig("helpful", map[string]any{"helpful": "You are helpful."})
	if got := ResolveEphemeralSystemPrompt(cfg); got != "You are helpful." {
		t.Fatalf("got %q", got)
	}
}

func TestResolveFallsBackToManualSystemPrompt(t *testing.T) {
	cfg := ephemeralPromptConfig("none", map[string]any{"helpful": "You are helpful."})
	if got := ResolveEphemeralSystemPrompt(cfg); got != "manual forever" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveIgnoresUnknownPersonalityName(t *testing.T) {
	cfg := ephemeralPromptConfig("missing", map[string]any{"helpful": "You are helpful."})
	if got := ResolveEphemeralSystemPrompt(cfg); got != "manual forever" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveRendersDictPersonality(t *testing.T) {
	cfg := ephemeralPromptConfig("coder", map[string]any{"coder": map[string]any{
		"system_prompt": "You are an expert programmer.", "tone": "technical", "style": "concise",
	}})
	resolved := ResolveEphemeralSystemPrompt(cfg)
	for _, want := range []string{"You are an expert programmer.", "Tone: technical", "Style: concise"} {
		if !strings.Contains(resolved, want) {
			t.Fatalf("resolved %q lacks %q", resolved, want)
		}
	}
}
