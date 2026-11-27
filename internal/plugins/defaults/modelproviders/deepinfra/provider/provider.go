package provider

import (
	deepinfraconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepinfra/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := deepinfraconfig.BaseURL
	return []provider.Spec{{
		Name:               deepinfraconfig.Name,
		Shape:              shape,
		DisplayName:        deepinfraconfig.Title,
		Description:        "Open models hosted by DeepInfra, with an API key paid by use",
		SignupURL:          "https://deepinfra.com/dash/api_keys",
		BaseURL:            base,
		BaseURLEnv:         deepinfraconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{deepinfraconfig.APIKeyEnv},
		Aliases:            []string{"deep-infra", "deepinfra-ai"},
		DefaultModel:       deepinfraconfig.DefaultModel,
		FallbackModels:     []string{},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		DefaultAuxModel:    "deepseek-ai/DeepSeek-V4-Flash",
	}}
}
