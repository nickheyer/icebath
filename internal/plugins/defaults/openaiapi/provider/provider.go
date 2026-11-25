package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	openaiapiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/openaiapi/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(openaiapiconfig.BaseURL)
	return []provider.Profile{{
		Name:               openaiapiconfig.Name,
		DisplayName:        "OpenAI API",
		BaseURL:            base,
		BaseURLEnv:         openaiapiconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeCodexResponses,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{openaiapiconfig.APIKeyEnv},
		DefaultModel:       p.config.GetString(openaiapiconfig.DefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        "openai",
		StaticModels: []string{
			"gpt-6.1-sol", "gpt-6.1-sol-pro", "gpt-6-sol", "gpt-6-sol-pro", "gpt-6-luna", "gpt-6-luna-pro",
			"gpt-5.6-sol", "gpt-5.6-sol-pro", "gpt-5.6-terra", "gpt-5.6-terra-pro", "gpt-5.6-luna",
			"gpt-5.6-luna-pro", "gpt-5.5", "gpt-5.5-pro", "gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano",
			"gpt-5-mini", "gpt-5.3-codex", "gpt-4.1", "gpt-4o", "gpt-4o-mini",
		},
	}}
}
