package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	openrouterconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/config"
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
		Name:               openrouterconfig.Name,
		DisplayName:        "OpenRouter",
		Description:        "OpenRouter — unified API for 200+ models",
		SignupURL:          "https://openrouter.ai/keys",
		BaseURL:            p.config.GetString(openrouterconfig.BaseURL),
		BaseURLEnv:         openrouterconfig.BaseURLEnv,
		ModelsURL:          openrouterconfig.ModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{openrouterconfig.APIKeyEnv},
		Aliases:            []string{"or", "openai"},
		DefaultModel:       p.config.GetString(openrouterconfig.DefaultModel),
		FallbackModels:     []string{"anthropic/claude-sonnet-4.6", "openai/gpt-5.4", "deepseek/deepseek-chat", "google/gemini-3.8-flash", "google/gemini-3.7-flash", "qwen/qwen3-plus"},
		Headers:            map[string]string{"HTTP-Referer": "https://github.com/nickheyer/icebath", "X-Title": "Icebath Agent"},
		Aggregator:         true,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        openrouterconfig.Name,
	}}
}
