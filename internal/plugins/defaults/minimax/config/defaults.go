package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(GlobalBaseURL, "https://api.minimax.io/anthropic")
	cfg.SetDefault(GlobalDefaultModel, "MiniMax-M3")
	cfg.MustBindEnv(GlobalBaseURL, GlobalBaseURLEnv)
	cfg.MustBindEnv(GlobalAPIKey, GlobalAPIKeyEnv)
	cfg.SetDefault(CNBaseURL, "https://api.minimaxi.com/anthropic")
	cfg.SetDefault(CNDefaultModel, "MiniMax-M3")
	cfg.MustBindEnv(CNBaseURL, CNBaseURLEnv)
	cfg.MustBindEnv(CNAPIKey, CNAPIKeyEnv)
	cfg.SetDefault(OAuthBaseURL, "https://api.minimax.io/anthropic")
	cfg.SetDefault(OAuthDefaultModel, "MiniMax-M2.7")
}
