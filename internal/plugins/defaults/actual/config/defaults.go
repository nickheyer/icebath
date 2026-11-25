package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(BaseURL, DefaultBaseURL)
	cfg.SetDefault(DefaultModel, "")
	cfg.MustBindEnv(BaseURL, BaseURLEnv)
	cfg.MustBindEnv(APIKey, APIKeyEnv)
}
