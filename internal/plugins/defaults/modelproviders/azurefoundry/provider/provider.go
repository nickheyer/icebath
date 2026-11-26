package provider

import (
	"strings"

	azurefoundryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/config"
	"github.com/nickheyer/icebath/internal/provider"
)

var responsesPrefixes = []string{"gpt-5", "codex", "gpt-codex", "o1", "o3", "o4"}

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := azurefoundryconfig.BaseURL
	return []provider.Spec{{
		Name:               azurefoundryconfig.Name,
		DisplayName:        azurefoundryconfig.Title,
		Description:        "OpenAI-compatible; set your endpoint as the base URL",
		SignupURL:          "https://ai.azure.com/",
		BaseURL:            base,
		BaseURLEnv:         azurefoundryconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{azurefoundryconfig.APIKeyEnv},
		Aliases:            []string{"azure", "azure-ai-foundry", "azure-ai"},
		DefaultModel:       azurefoundryconfig.DefaultModel,
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
