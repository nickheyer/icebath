package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(BaseURL, "https://api.openai.com/v1")
	cfg.SetDefault(DefaultModel, "")
	cfg.MustBindEnv(BaseURL, BaseURLEnv)
	cfg.MustBindEnv(APIKey, APIKeyEnv)
}
