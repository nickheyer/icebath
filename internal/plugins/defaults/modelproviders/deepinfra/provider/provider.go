package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	deepinfraconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepinfra/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(deepinfraconfig.BaseURL)
	return []provider.Profile{{
		Name:               deepinfraconfig.Name,
		DisplayName:        "DeepInfra",
		Description:        "DeepInfra - 100+ open models, pay-per-use",
		SignupURL:          "https://deepinfra.com/dash/api_keys",
		BaseURL:            base,
		BaseURLEnv:         deepinfraconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{deepinfraconfig.APIKeyEnv},
		Aliases:            []string{"deep-infra", "deepinfra-ai"},
		DefaultModel:       p.config.GetString(deepinfraconfig.DefaultModel),
		FallbackModels:     []string{},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        deepinfraconfig.Name,
		DefaultAuxModel:    "deepseek-ai/DeepSeek-V4-Flash",
	}}
}
