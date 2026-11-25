package provider

import (
	"testing"

	"github.com/nickheyer/icebath/internal/config"
	githubcopilotconfig "github.com/nickheyer/icebath/internal/plugins/defaults/githubcopilot/config"
	"github.com/nickheyer/icebath/internal/provider"
)

func TestCopilotRoutesGPT5AndLaterThroughResponses(t *testing.T) {
	t.Setenv(githubcopilotconfig.CopilotBaseURLEnv, "")
	t.Setenv(githubcopilotconfig.ACPBaseURLEnv, "")
	cfg := config.New(t.TempDir())
	githubcopilotconfig.Register(cfg)

	listed := profiles{config: cfg}.Profiles()
	if len(listed) != 2 || listed[0].Name != githubcopilotconfig.CopilotName || listed[1].Name != githubcopilotconfig.ACPName {
		t.Fatalf("Profiles() returned %d profiles, want copilot then copilot-acp", len(listed))
	}
	copilot, acp := listed[0], listed[1]
	if copilot.BaseURL != "https://api.githubcopilot.com" || copilot.ModelsURL != "https://api.githubcopilot.com/models" || copilot.Auth != provider.AuthAPIKey {
		t.Fatalf("copilot = %q, %q, %q, want the Copilot API on an api key", copilot.BaseURL, copilot.ModelsURL, copilot.Auth)
	}
	if acp.BaseURL != githubcopilotconfig.ACPMarkerURL || acp.Auth != provider.AuthOAuth || acp.AuthType != provider.AuthTypeExternalProcess || acp.PerModelMode != nil {
		t.Fatalf("copilot-acp = %q, %q, %q, want the ACP marker on an external process", acp.BaseURL, acp.Auth, acp.AuthType)
	}
	if copilot.PerModelMode == nil {
		t.Fatal("copilot has no per model mode")
	}
	for model, want := range map[string]provider.APIMode{
		"gpt-5.4":           provider.ModeCodexResponses,
		"gpt-5.3-codex":     provider.ModeCodexResponses,
		"gpt-6":             provider.ModeCodexResponses,
		"gpt-10-preview":    provider.ModeCodexResponses,
		"gpt-5-mini":        provider.ModeChatCompletions,
		"gpt-5-mini-2026":   provider.ModeChatCompletions,
		"gpt-4.1":           provider.ModeChatCompletions,
		"gpt-4o":            provider.ModeChatCompletions,
		"claude-sonnet-4.6": provider.ModeChatCompletions,
		"openai/gpt-5.4":    provider.ModeChatCompletions,
		"":                  provider.ModeChatCompletions,
	} {
		if got := copilot.PerModelMode(model); got != want {
			t.Errorf("PerModelMode(%q) = %q, want %q", model, got, want)
		}
	}
}
