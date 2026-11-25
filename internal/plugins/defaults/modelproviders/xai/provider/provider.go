package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	xaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(xaiconfig.XAIBaseURL)
	oauthBase := p.config.GetString(xaiconfig.OAuthBaseURL)
	return []provider.Profile{{
		Name:               xaiconfig.XAIName,
		DisplayName:        "xAI",
		BaseURL:            base,
		BaseURLEnv:         xaiconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeCodexResponses,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{xaiconfig.APIKeyEnv},
		Aliases:            []string{"grok", "x-ai", "x.ai"},
		DefaultModel:       p.config.GetString(xaiconfig.XAIDefaultModel),
		Headers:            map[string]string{"User-Agent": "Icebath-Agent/dev"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        xaiconfig.XAIName,
	}, {
		Name:               xaiconfig.OAuthName,
		DisplayName:        "xAI Grok OAuth (SuperGrok / Premium+)",
		BaseURL:            oauthBase,
		BaseURLEnv:         xaiconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(oauthBase),
		Mode:               provider.ModeCodexResponses,
		Auth:               provider.AuthOAuth,
		AuthType:           provider.AuthTypeOAuthExternal,
		Aliases:            []string{"grok-oauth", "x-ai-oauth", "xai-grok-oauth"},
		DefaultModel:       p.config.GetString(xaiconfig.OAuthDefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        xaiconfig.OAuthName,
	}}
}
