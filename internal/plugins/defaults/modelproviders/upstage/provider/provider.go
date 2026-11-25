package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	upstageconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/upstage/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	return []provider.Profile{{
		Name:               upstageconfig.Name,
		DisplayName:        "Upstage Solar",
		Description:        "Upstage (Solar API)",
		SignupURL:          "https://console.upstage.ai/api-keys",
		BaseURL:            p.config.GetString(upstageconfig.BaseURL),
		BaseURLEnv:         upstageconfig.BaseURLEnv,
		ModelsURL:          upstageconfig.ModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{upstageconfig.APIKeyEnv},
		Aliases:            []string{"solar"},
		DefaultModel:       p.config.GetString(upstageconfig.DefaultModel),
		FallbackModels:     []string{"solar-pro3"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        upstageconfig.Name,
	}}
}
