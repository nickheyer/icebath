package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(BaseURL, "https://api.z.ai/api/paas/v4")
	cfg.SetDefault(DefaultModel, "glm-4.5-flash")
	cfg.MustBindEnv(BaseURL, BaseURLEnv)
	cfg.MustBindEnv(APIKey, APIKeyEnv, ZAIAPIKeyEnv, ZDotAIAPIKeyEnv)
}
