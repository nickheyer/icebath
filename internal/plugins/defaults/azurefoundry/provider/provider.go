package provider

import (
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	azurefoundryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/azurefoundry/config"
	"github.com/nickheyer/icebath/internal/provider"
)

var responsesPrefixes = []string{"gpt-5", "codex", "gpt-codex", "o1", "o3", "o4"}

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(azurefoundryconfig.BaseURL)
	return []provider.Profile{{
		Name:               azurefoundryconfig.Name,
		DisplayName:        "Azure Foundry",
		Description:        "Microsoft Foundry - OpenAI-compatible endpoint (user-supplied base URL)",
		SignupURL:          "https://ai.azure.com/",
		BaseURL:            base,
		BaseURLEnv:         azurefoundryconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{azurefoundryconfig.APIKeyEnv},
		Aliases:            []string{"azure", "azure-ai-foundry", "azure-ai"},
		DefaultModel:       p.config.GetString(azurefoundryconfig.DefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        azurefoundryconfig.Name,
		PerModelMode:       mode,
	}}
}

func mode(model string) provider.APIMode {
	raw := strings.ToLower(strings.TrimSpace(model))
	raw = raw[strings.LastIndex(raw, "/")+1:]
	if raw == "" {
		return ""
	}
	for _, prefix := range responsesPrefixes {
		if strings.HasPrefix(raw, prefix) {
			return provider.ModeCodexResponses
		}
	}
	return ""
}
