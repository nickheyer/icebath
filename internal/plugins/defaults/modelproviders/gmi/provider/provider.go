package provider

import (
	gmiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gmi/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := gmiconfig.BaseURL
	return []provider.Spec{{
		Name:               gmiconfig.Name,
		DisplayName:        gmiconfig.Title,
		Description:        "Multi-model API with provider/model IDs",
		SignupURL:          "https://www.gmicloud.ai/",
		BaseURL:            base,
		BaseURLEnv:         gmiconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{gmiconfig.APIKeyEnv},
		Aliases:            []string{"gmi-cloud", "gmicloud"},
		DefaultModel:       gmiconfig.DefaultModel,
		FallbackModels:     []string{"zai-org/GLM-5.1-FP8", "deepseek-ai/DeepSeek-V3.2", "moonshotai/Kimi-K2.5", "google/gemini-3.1-flash-lite-preview", "anthropic/claude-sonnet-5", "anthropic/claude-sonnet-4.6", "openai/gpt-5.4"},
		Headers:            map[string]string{"User-Agent": provider.UserAgent("IcebathAgent")},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		StaticModels:       []string{"zai-org/GLM-5.1-FP8", "deepseek-ai/DeepSeek-V3.2", "moonshotai/Kimi-K2.5", "google/gemini-3.1-flash-lite-preview", "anthropic/claude-sonnet-5", "anthropic/claude-sonnet-4.6", "openai/gpt-5.4"},
		ListsContext:       true,
		DefaultAuxModel:    "google/gemini-3.1-flash-lite-preview",
	}}
}
