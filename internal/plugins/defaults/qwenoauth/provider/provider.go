package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	qwenoauthconfig "github.com/nickheyer/icebath/internal/plugins/defaults/qwenoauth/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(qwenoauthconfig.BaseURL)
	return []provider.Profile{{
		Name:               qwenoauthconfig.Name,
		DisplayName:        "Qwen OAuth",
		BaseURL:            base,
		BaseURLEnv:         qwenoauthconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthOAuth,
		AuthType:           provider.AuthTypeOAuthExternal,
		KeyEnv:             []string{qwenoauthconfig.APIKeyEnv},
		Aliases:            []string{"qwen", "qwen-portal", "qwen-cli"},
		DefaultModel:       p.config.GetString(qwenoauthconfig.DefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		DefaultMaxTokens:   65536,
		ModelsDevID:        "alibaba",
	}}
}
