package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	huggingfaceconfig "github.com/nickheyer/icebath/internal/plugins/defaults/huggingface/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(huggingfaceconfig.BaseURL)
	return []provider.Profile{{
		Name:               huggingfaceconfig.Name,
		DisplayName:        "Hugging Face",
		Description:        "HuggingFace Inference API",
		SignupURL:          "https://huggingface.co/settings/tokens",
		BaseURL:            base,
		BaseURLEnv:         huggingfaceconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{huggingfaceconfig.APIKeyEnv},
		Aliases:            []string{"hf", "hugging-face", "huggingface-hub"},
		DefaultModel:       p.config.GetString(huggingfaceconfig.DefaultModel),
		FallbackModels:     []string{"Qwen/Qwen3.5-72B-Instruct", "deepseek-ai/DeepSeek-V3.2"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        huggingfaceconfig.Name,
	}}
}
