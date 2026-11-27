package provider

import (
	nebiustokenfactoryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nebiustokenfactory/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:            nebiustokenfactoryconfig.Name,
		Shape:           shape,
		Native:          provider.Unprefixed,
		DisplayName:     nebiustokenfactoryconfig.Title,
		Description:     "Open models hosted by Nebius, with an API key paid by use",
		SignupURL:       "https://tokenfactory.nebius.com/",
		BaseURL:         nebiustokenfactoryconfig.BaseURL,
		BaseURLEnv:      nebiustokenfactoryconfig.BaseURLEnv,
		ModelsURL:       nebiustokenfactoryconfig.ModelsURL,
		Mode:            provider.ModeChatCompletions,
		Auth:            provider.AuthAPIKey,
		AuthType:        provider.AuthTypeAPIKey,
		KeyEnv:          []string{nebiustokenfactoryconfig.APIKeyEnv, nebiustokenfactoryconfig.TokenFactoryAPIKeyEnv},
		Aliases:         []string{"nebius", "nebius-tokenfactory", "nebius-tf", "token-factory", "tokenfactory"},
		DefaultModel:    nebiustokenfactoryconfig.DefaultModel,
		DefaultAuxModel: "nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B",
		FallbackModels: []string{"Qwen/Qwen3.5-397B-A17B-fast", "deepseek-ai/DeepSeek-V4-Pro", "zai-org/GLM-5.1", "moonshotai/Kimi-K2.5-fast",
			"MiniMaxAI/MiniMax-M2.5-fast", "deepseek-ai/DeepSeek-V3.2-fast", "openai/gpt-oss-120b-fast", "meta-llama/Llama-3.3-70B-Instruct"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
	}}
}
