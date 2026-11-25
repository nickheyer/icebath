package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(MaxTokens, 64000)
	cfg.SetDefault(Fallbacks, true)
}
