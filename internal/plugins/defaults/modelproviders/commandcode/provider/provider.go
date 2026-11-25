package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	commandcodeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/commandcode/config"
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
		Name:            commandcodeconfig.ChatName,
		DisplayName:     "CommandCode",
		Description:     "CommandCode — 20+ models via OpenAI-compatible API",
		SignupURL:       "https://commandcode.ai/",
		BaseURL:         p.config.GetString(commandcodeconfig.ChatBaseURL),
		BaseURLEnv:      commandcodeconfig.ChatBaseURLEnv,
		ModelsURL:       commandcodeconfig.ModelsURL,
		Mode:            provider.ModeChatCompletions,
		Auth:            provider.AuthAPIKey,
		AuthType:        provider.AuthTypeAPIKey,
		KeyEnv:          []string{commandcodeconfig.APIKeyEnv},
		Aliases:         []string{"commandcode-chat"},
		DefaultModel:    p.config.GetString(commandcodeconfig.ChatDefaultModel),
		DefaultAuxModel: "deepseek/deepseek-v4-flash",
		FallbackModels: []string{"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash", "Qwen/Qwen3.7-Max", "Qwen/Qwen3.6-Plus",
			"moonshotai/Kimi-K2.6", "zai-org/GLM-5.1", "MiniMaxAI/MiniMax-M2.7", "stepfun/Step-3.5-Flash",
			"xiaomi/mimo-v2.5-pro", "google/gemini-3.5-flash", "gpt-5.5"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        commandcodeconfig.ChatName,
	}, {
		Name:               commandcodeconfig.AnthropicName,
		DisplayName:        "CommandCode (Anthropic)",
		Description:        "CommandCode — Claude models via Anthropic Messages API",
		SignupURL:          "https://commandcode.ai/",
		BaseURL:            p.config.GetString(commandcodeconfig.AnthropicBaseURL),
		BaseURLEnv:         commandcodeconfig.AnthropicBaseURLEnv,
		ModelsURL:          commandcodeconfig.ModelsURL,
		Mode:               provider.ModeAnthropicMessages,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{commandcodeconfig.APIKeyEnv},
		Aliases:            []string{"commandcode-claude"},
		DefaultModel:       p.config.GetString(commandcodeconfig.AnthropicDefaultModel),
		DefaultAuxModel:    "claude-haiku-4-5-20251001",
		FallbackModels:     []string{"claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5-20251001"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        commandcodeconfig.AnthropicName,
	}}
}
