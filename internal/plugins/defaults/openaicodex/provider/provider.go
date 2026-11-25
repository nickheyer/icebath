package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	openaicodexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/openaicodex/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(openaicodexconfig.BaseURL)
	return []provider.Profile{{
		Name:               openaicodexconfig.Name,
		DisplayName:        "ChatGPT or Codex Subscription",
		BaseURL:            base,
		BaseURLEnv:         openaicodexconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeCodexResponses,
		Auth:               provider.AuthOAuth,
		AuthType:           provider.AuthTypeOAuthExternal,
		Aliases:            []string{"codex", "openai_codex", "chatgpt", "chatgpt-codex"},
		DefaultModel:       p.config.GetString(openaicodexconfig.DefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        openaicodexconfig.Name,
	}}
}
