package provider

import (
	xaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := xaiconfig.XAIBaseURL
	oauthBase := xaiconfig.OAuthBaseURL
	return []provider.Spec{{
		Name:               xaiconfig.XAIName,
		DisplayName:        xaiconfig.XAITitle,
		BaseURL:            base,
		BaseURLEnv:         xaiconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeCodexResponses,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{xaiconfig.APIKeyEnv},
		Aliases:            []string{"grok", "x-ai", "x.ai"},
		DefaultModel:       xaiconfig.XAIDefaultModel,
		Headers:            map[string]string{"User-Agent": "Icebath-Agent/dev"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        xaiconfig.XAIName,
	}, {
		Name:               xaiconfig.OAuthName,
		DisplayName:        xaiconfig.OAuthTitle,
		BaseURL:            oauthBase,
		BaseURLEnv:         xaiconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(oauthBase),
		Mode:               provider.ModeCodexResponses,
		Auth:               provider.AuthOAuth,
		AuthType:           provider.AuthTypeOAuthExternal,
		Aliases:            []string{"grok-oauth", "x-ai-oauth", "xai-grok-oauth"},
		DefaultModel:       xaiconfig.OAuthDefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        xaiconfig.OAuthName,
	}}
}
