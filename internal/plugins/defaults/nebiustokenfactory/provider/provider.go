package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	nebiustokenfactoryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/nebiustokenfactory/config"
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
		Name:            nebiustokenfactoryconfig.Name,
		DisplayName:     "Nebius Token Factory",
		Description:     "Nebius Token Factory — OpenAI-compatible inference",
		SignupURL:       "https://tokenfactory.nebius.com/",
		BaseURL:         p.config.GetString(nebiustokenfactoryconfig.BaseURL),
		BaseURLEnv:      nebiustokenfactoryconfig.BaseURLEnv,
		ModelsURL:       nebiustokenfactoryconfig.ModelsURL,
		Mode:            provider.ModeChatCompletions,
		Auth:            provider.AuthAPIKey,
		AuthType:        provider.AuthTypeAPIKey,
		KeyEnv:          []string{nebiustokenfactoryconfig.APIKeyEnv, nebiustokenfactoryconfig.TokenFactoryAPIKeyEnv},
		Aliases:         []string{"nebius", "nebius-tokenfactory", "nebius-tf", "token-factory", "tokenfactory"},
		DefaultModel:    p.config.GetString(nebiustokenfactoryconfig.DefaultModel),
		DefaultAuxModel: "nvidia/NVIDIA-Nemotron-3-Nano-30B-A3B",
		FallbackModels: []string{"Qwen/Qwen3.5-397B-A17B-fast", "deepseek-ai/DeepSeek-V4-Pro", "zai-org/GLM-5.1", "moonshotai/Kimi-K2.5-fast",
			"MiniMaxAI/MiniMax-M2.5-fast", "deepseek-ai/DeepSeek-V3.2-fast", "openai/gpt-oss-120b-fast", "meta-llama/Llama-3.3-70B-Instruct"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        nebiustokenfactoryconfig.Name,
	}}
}
