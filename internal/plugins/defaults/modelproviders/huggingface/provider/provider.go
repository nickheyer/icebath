package provider

import (
	huggingfaceconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/huggingface/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := huggingfaceconfig.BaseURL
	return []provider.Spec{{
		Name:               huggingfaceconfig.Name,
		DisplayName:        huggingfaceconfig.Title,
		Description:        "Inference API",
		SignupURL:          "https://huggingface.co/settings/tokens",
		BaseURL:            base,
		BaseURLEnv:         huggingfaceconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{huggingfaceconfig.APIKeyEnv},
		Aliases:            []string{"hf", "hugging-face", "huggingface-hub"},
		DefaultModel:       huggingfaceconfig.DefaultModel,
		FallbackModels:     []string{"Qwen/Qwen3.5-72B-Instruct", "deepseek-ai/DeepSeek-V3.2"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        huggingfaceconfig.Name,
		StaticModels:       []string{"moonshotai/Kimi-K2.5", "Qwen/Qwen3.5-397B-A17B", "Qwen/Qwen3.5-35B-A3B", "deepseek-ai/DeepSeek-V3.2", "MiniMaxAI/MiniMax-M2.5", "zai-org/GLM-5", "XiaomiMiMo/MiMo-V2-Flash", "moonshotai/Kimi-K2-Thinking", "moonshotai/Kimi-K2.6"},
		ModelsDevPreferred: true,
	}}
}
