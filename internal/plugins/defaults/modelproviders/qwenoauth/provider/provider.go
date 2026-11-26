package provider

import (
	qwenoauthconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := qwenoauthconfig.BaseURL
	return []provider.Spec{{
		Name:               qwenoauthconfig.Name,
		DisplayName:        qwenoauthconfig.Title,
		BaseURL:            base,
		BaseURLEnv:         qwenoauthconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthOAuth,
		AuthType:           provider.AuthTypeOAuthExternal,
		KeyEnv:             []string{qwenoauthconfig.APIKeyEnv},
		Aliases:            []string{"qwen", "qwen-portal", "qwen-cli"},
		DefaultModel:       qwenoauthconfig.DefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		DefaultMaxTokens:   65536,
		ModelsDevID:        "alibaba",
	}}
}
