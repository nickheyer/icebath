package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(BaseURL, "https://bedrock-runtime.us-east-1.amazonaws.com")
	cfg.SetDefault(DefaultModel, "")
	cfg.MustBindEnv(BaseURL, BaseURLEnv)
	cfg.MustBindEnv(APIKey, APIKeyEnv)
}
