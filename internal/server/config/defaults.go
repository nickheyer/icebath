package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(Address, "127.0.0.1:7433")
}
