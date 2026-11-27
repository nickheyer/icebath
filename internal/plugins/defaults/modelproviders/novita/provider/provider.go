package provider

import (
	novitaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/novita/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := novitaconfig.BaseURL
	return []provider.Spec{{
		Name:               novitaconfig.Name,
		Carriers:           []string{"reasoning_content", "reasoning_details"},
		DisplayName:        novitaconfig.Title,
		Description:        "Model inference cloud",
		SignupURL:          "https://novita.ai/settings/key-management",
		BaseURL:            base,
		BaseURLEnv:         novitaconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{novitaconfig.APIKeyEnv},
		Aliases:            []string{"novita-ai", "novitaai"},
		DefaultModel:       novitaconfig.DefaultModel,
		FallbackModels:     []string{"moonshotai/kimi-k2.5", "minimax/minimax-m2.7", "zai-org/glm-5", "deepseek/deepseek-v3-0324", "deepseek/deepseek-r1-0528", "qwen/qwen3-235b-a22b-fp8"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        "novita-ai",
		StaticModels:       []string{"moonshotai/kimi-k2.5", "minimax/minimax-m2.7", "zai-org/glm-5", "deepseek/deepseek-v3-0324", "deepseek/deepseek-r1-0528", "qwen/qwen3-235b-a22b-fp8"},
		ListsContext:       true,
		DefaultAuxModel:    "deepseek/deepseek-v3-0324",
	}}
}
