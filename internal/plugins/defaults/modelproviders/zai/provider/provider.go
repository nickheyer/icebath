package provider

import (
	zaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/zai/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:               zaiconfig.Name,
		DisplayName:        zaiconfig.Title,
		Description:        "GLM models from Zhipu AI",
		SignupURL:          "https://z.ai/",
		BaseURL:            zaiconfig.BaseURL,
		BaseURLEnv:         zaiconfig.BaseURLEnv,
		ModelsURL:          zaiconfig.ModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{zaiconfig.APIKeyEnv, zaiconfig.ZAIAPIKeyEnv, zaiconfig.ZDotAIAPIKeyEnv},
		Aliases:            []string{"glm", "z-ai", "z.ai", "zhipu"},
		DefaultModel:       zaiconfig.DefaultModel,
		DefaultAuxModel:    "glm-4.5-flash",
		FallbackModels:     []string{"glm-5.2", "glm-5", "glm-4-9b"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        zaiconfig.Name,
	}}
}
