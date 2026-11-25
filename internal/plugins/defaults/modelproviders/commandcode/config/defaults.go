package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(ChatBaseURL, Endpoint)
	cfg.SetDefault(ChatDefaultModel, "")
	cfg.MustBindEnv(ChatBaseURL, ChatBaseURLEnv)
	cfg.MustBindEnv(ChatAPIKey, APIKeyEnv)
	cfg.SetDefault(AnthropicBaseURL, Endpoint)
	cfg.SetDefault(AnthropicDefaultModel, "")
	cfg.MustBindEnv(AnthropicBaseURL, AnthropicBaseURLEnv)
	cfg.MustBindEnv(AnthropicAPIKey, APIKeyEnv)
}
