package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(BaseURL, "https://generativelanguage.googleapis.com/v1beta")
	cfg.SetDefault(DefaultModel, "gemini-3.6-flash")
	cfg.MustBindEnv(BaseURL, BaseURLEnv)
	cfg.MustBindEnv(APIKey, GoogleAPIKeyEnv, GeminiAPIKeyEnv)
}
