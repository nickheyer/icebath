package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(Timeout, 180)
	cfg.SetDefault(EnvPassthrough, []string{})
}
