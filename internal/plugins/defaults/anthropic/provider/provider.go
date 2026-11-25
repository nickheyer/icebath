package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	anthropicconfig "github.com/nickheyer/icebath/internal/plugins/defaults/anthropic/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(anthropicconfig.BaseURL)
	return []provider.Profile{{
		Name:               anthropicconfig.Name,
		DisplayName:        "Anthropic",
		BaseURL:            base,
		BaseURLEnv:         anthropicconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeAnthropicMessages,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{anthropicconfig.APIKeyEnv, anthropicconfig.TokenEnv, anthropicconfig.OAuthTokenEnv},
		Aliases:            []string{"claude", "claude-oauth", "claude-code"},
		DefaultModel:       p.config.GetString(anthropicconfig.DefaultModel),
		DefaultAuxModel:    "claude-haiku-4-5-20251001",
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        anthropicconfig.Name,
	}}
}
