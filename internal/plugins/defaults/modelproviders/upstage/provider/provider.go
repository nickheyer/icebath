package provider

import (
	upstageconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/upstage/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:               upstageconfig.Name,
		Shape:              shape,
		Carriers:           []string{"reasoning_content", "reasoning"},
		DisplayName:        upstageconfig.Title,
		Description:        "Upstage's Solar models, with an API key paid by use",
		SignupURL:          "https://console.upstage.ai/api-keys",
		BaseURL:            upstageconfig.BaseURL,
		BaseURLEnv:         upstageconfig.BaseURLEnv,
		ModelsURL:          upstageconfig.ModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{upstageconfig.APIKeyEnv},
		Aliases:            []string{"solar"},
		DefaultModel:       upstageconfig.DefaultModel,
		FallbackModels:     []string{"solar-pro3"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
	}}
}
