package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	zaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/zai/config"
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
		Name:               zaiconfig.Name,
		DisplayName:        "Z.AI (GLM)",
		Description:        "Z.AI / GLM - Zhipu AI models",
		SignupURL:          "https://z.ai/",
		BaseURL:            p.config.GetString(zaiconfig.BaseURL),
		BaseURLEnv:         zaiconfig.BaseURLEnv,
		ModelsURL:          zaiconfig.ModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{zaiconfig.APIKeyEnv, zaiconfig.ZAIAPIKeyEnv, zaiconfig.ZDotAIAPIKeyEnv},
		Aliases:            []string{"glm", "z-ai", "z.ai", "zhipu"},
		DefaultModel:       p.config.GetString(zaiconfig.DefaultModel),
		DefaultAuxModel:    "glm-4.5-flash",
		FallbackModels:     []string{"glm-5.2", "glm-5", "glm-4-9b"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        zaiconfig.Name,
	}}
}
