package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	novitaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/novita/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(novitaconfig.BaseURL)
	return []provider.Profile{{
		Name:               novitaconfig.Name,
		DisplayName:        "NovitaAI",
		Description:        "NovitaAI — AI-native cloud for builders and agents",
		SignupURL:          "https://novita.ai/settings/key-management",
		BaseURL:            base,
		BaseURLEnv:         novitaconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{novitaconfig.APIKeyEnv},
		Aliases:            []string{"novita-ai", "novitaai"},
		DefaultModel:       p.config.GetString(novitaconfig.DefaultModel),
		FallbackModels:     []string{"moonshotai/kimi-k2.5", "minimax/minimax-m2.7", "zai-org/glm-5", "deepseek/deepseek-v3-0324", "deepseek/deepseek-r1-0528", "qwen/qwen3-235b-a22b-fp8"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        novitaconfig.Name,
		DefaultAuxModel:    "deepseek/deepseek-v3-0324",
	}}
}
