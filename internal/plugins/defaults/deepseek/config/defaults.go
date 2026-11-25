package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(BaseURL, "https://api.deepseek.com/v1")
	cfg.SetDefault(DefaultModel, "deepseek-flash")
	cfg.MustBindEnv(BaseURL, BaseURLEnv)
	cfg.MustBindEnv(APIKey, APIKeyEnv)
}
