package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(GlobalBaseURL, GlobalEndpoint)
	cfg.SetDefault(GlobalDefaultModel, "kimi-k2-turbo-preview")
	cfg.MustBindEnv(GlobalBaseURL, GlobalBaseURLEnv)
	cfg.MustBindEnv(GlobalAPIKey, GlobalAPIKeyEnv, GlobalCodingAPIKeyEnv)
	cfg.SetDefault(CNBaseURL, CNEndpoint)
	cfg.SetDefault(CNDefaultModel, "kimi-k2-turbo-preview")
	cfg.MustBindEnv(CNAPIKey, CNAPIKeyEnv)
}
