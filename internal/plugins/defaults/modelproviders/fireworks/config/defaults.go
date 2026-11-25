package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(BaseURL, "https://api.fireworks.ai/inference/v1")
	cfg.SetDefault(DefaultModel, "")
	cfg.MustBindEnv(APIKey, APIKeyEnv)
}
