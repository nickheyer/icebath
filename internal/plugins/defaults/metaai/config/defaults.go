package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(BaseURL, "https://api.meta.ai/v1")
	cfg.SetDefault(DefaultModel, "")
	cfg.MustBindEnv(BaseURL, BaseURLEnv)
	cfg.MustBindEnv(APIKey, APIKeyEnv, MetaAPIKeyEnv, MetaModelAPIKeyEnv)
}
