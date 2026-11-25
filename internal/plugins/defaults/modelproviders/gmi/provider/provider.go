package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	gmiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gmi/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(gmiconfig.BaseURL)
	return []provider.Profile{{
		Name:               gmiconfig.Name,
		DisplayName:        "GMI Cloud",
		Description:        "GMI Cloud - multi-model direct API (slash-form model IDs)",
		SignupURL:          "https://www.gmicloud.ai/",
		BaseURL:            base,
		BaseURLEnv:         gmiconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{gmiconfig.APIKeyEnv},
		Aliases:            []string{"gmi-cloud", "gmicloud"},
		DefaultModel:       p.config.GetString(gmiconfig.DefaultModel),
		FallbackModels:     []string{"zai-org/GLM-5.1-FP8", "deepseek-ai/DeepSeek-V3.2", "moonshotai/Kimi-K2.5", "google/gemini-3.1-flash-lite-preview", "anthropic/claude-sonnet-5", "anthropic/claude-sonnet-4.6", "openai/gpt-5.4"},
		Headers:            map[string]string{"User-Agent": "IcebathAgent/dev"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        gmiconfig.Name,
		DefaultAuxModel:    "google/gemini-3.1-flash-lite-preview",
	}}
}
