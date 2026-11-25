package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	deepseekconfig "github.com/nickheyer/icebath/internal/plugins/defaults/deepseek/config"
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
		Name:                       deepseekconfig.Name,
		DisplayName:                "DeepSeek",
		Description:                "DeepSeek — native DeepSeek API",
		SignupURL:                  "https://platform.deepseek.com/",
		BaseURL:                    p.config.GetString(deepseekconfig.BaseURL),
		BaseURLEnv:                 deepseekconfig.BaseURLEnv,
		ModelsURL:                  deepseekconfig.ModelsURL,
		Mode:                       provider.ModeChatCompletions,
		Auth:                       provider.AuthAPIKey,
		AuthType:                   provider.AuthTypeAPIKey,
		KeyEnv:                     []string{deepseekconfig.APIKeyEnv},
		Aliases:                    []string{"deepseek-chat", "deep-seek"},
		DefaultModel:               p.config.GetString(deepseekconfig.DefaultModel),
		DefaultAuxModel:            "deepseek-flash",
		FallbackModels:             []string{"deepseek-v4-pro", "deepseek-flash"},
		ModelListing:               true,
		HealthCheck:                true,
		VisionToolMessages:         true,
		UnsupportedResponseFormats: []string{"json_schema"},
		ModelsDevID:                deepseekconfig.Name,
	}}
}
