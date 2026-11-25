package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(XAIBaseURL, "https://api.x.ai/v1")
	cfg.SetDefault(XAIDefaultModel, "")
	cfg.MustBindEnv(XAIBaseURL, BaseURLEnv)
	cfg.MustBindEnv(XAIAPIKey, APIKeyEnv)
	cfg.SetDefault(OAuthBaseURL, "https://api.x.ai/v1")
	cfg.SetDefault(OAuthDefaultModel, "")
	cfg.MustBindEnv(OAuthBaseURL, BaseURLEnv)
}
