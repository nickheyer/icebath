package provider

import (
	commandcodeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/commandcode/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:            commandcodeconfig.ChatName,
		Shape:           shape,
		DisplayName:     commandcodeconfig.ChatTitle,
		Description:     "20+ models, OpenAI-compatible API",
		SignupURL:       "https://commandcode.ai/",
		BaseURL:         commandcodeconfig.ChatBaseURL,
		BaseURLEnv:      commandcodeconfig.ChatBaseURLEnv,
		ModelsURL:       commandcodeconfig.ModelsURL,
		Mode:            provider.ModeChatCompletions,
		Auth:            provider.AuthAPIKey,
		AuthType:        provider.AuthTypeAPIKey,
		KeyEnv:          []string{commandcodeconfig.APIKeyEnv},
		Aliases:         []string{"commandcode-chat"},
		DefaultModel:    commandcodeconfig.ChatDefaultModel,
		DefaultAuxModel: "deepseek/deepseek-v4-flash",
		FallbackModels: []string{"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash", "Qwen/Qwen3.7-Max", "Qwen/Qwen3.6-Plus",
			"moonshotai/Kimi-K2.6", "zai-org/GLM-5.1", "MiniMaxAI/MiniMax-M2.7", "stepfun/Step-3.5-Flash",
			"xiaomi/mimo-v2.5-pro", "google/gemini-3.5-flash", "gpt-5.5"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ListsContext:       true,
	}, {
		Name:               commandcodeconfig.AnthropicName,
		Shape:              shape,
		DisplayName:        commandcodeconfig.AnthropicTitle,
		Description:        "Claude models via the Anthropic API",
		SignupURL:          "https://commandcode.ai/",
		BaseURL:            commandcodeconfig.AnthropicBaseURL,
		BaseURLEnv:         commandcodeconfig.AnthropicBaseURLEnv,
		ModelsURL:          commandcodeconfig.ModelsURL,
		Mode:               provider.ModeAnthropicMessages,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{commandcodeconfig.APIKeyEnv},
		Aliases:            []string{"commandcode-claude"},
		DefaultModel:       commandcodeconfig.AnthropicDefaultModel,
		DefaultAuxModel:    "claude-haiku-4-5-20251001",
		FallbackModels:     []string{"claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5-20251001"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ListsContext:       true,
	}}
}
