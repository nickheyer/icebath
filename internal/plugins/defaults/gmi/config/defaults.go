package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(BaseURL, "https://api.gmi-serving.com/v1")
	cfg.SetDefault(DefaultModel, "google/gemini-3.1-flash-lite-preview")
	cfg.MustBindEnv(BaseURL, BaseURLEnv)
	cfg.MustBindEnv(APIKey, APIKeyEnv)
}
