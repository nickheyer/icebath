package provider

import (
	"testing"

	"github.com/nickheyer/icebath/internal/config"
	azurefoundryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/azurefoundry/config"
	"github.com/nickheyer/icebath/internal/provider"
)

func TestProfileRoutesResponsesOnlyDeployments(t *testing.T) {
	t.Setenv(azurefoundryconfig.BaseURLEnv, "https://resource.openai.azure.com/openai/v1/")
	cfg := config.New(t.TempDir())
	azurefoundryconfig.Register(cfg)

	listed := profiles{config: cfg}.Profiles()
	if len(listed) != 1 || listed[0].PerModelMode == nil {
		t.Fatalf("Profiles() returned %d profiles, want one with a per model mode", len(listed))
	}
	profile := listed[0]
	if profile.BaseURL != "https://resource.openai.azure.com/openai/v1/" || profile.ModelsURL != "https://resource.openai.azure.com/openai/v1/models" {
		t.Fatalf("BaseURL, ModelsURL = %q, %q, want the user supplied resource and its models endpoint", profile.BaseURL, profile.ModelsURL)
	}
	for model, want := range map[string]provider.APIMode{
		"gpt-5.4":                  provider.ModeCodexResponses,
		" Deployments/GPT-5-mini ": provider.ModeCodexResponses,
		"codex-mini":               provider.ModeCodexResponses,
		"gpt-codex-1":              provider.ModeCodexResponses,
		"o1-preview":               provider.ModeCodexResponses,
		"o3":                       provider.ModeCodexResponses,
		"o4-mini":                  provider.ModeCodexResponses,
		"gpt-4o":                   "",
		"llama-3.3-70b":            "",
		"deployment/":              "",
		"":                         "",
	} {
		if got := profile.PerModelMode(model); got != want {
			t.Errorf("PerModelMode(%q) = %q, want %q", model, got, want)
		}
	}

	t.Setenv(azurefoundryconfig.BaseURLEnv, "")
	if profile := (profiles{config: cfg}).Profiles()[0]; profile.BaseURL != "" || profile.ModelsURL != "" {
		t.Fatalf("BaseURL, ModelsURL = %q, %q, want both empty without a resource", profile.BaseURL, profile.ModelsURL)
	}
}
