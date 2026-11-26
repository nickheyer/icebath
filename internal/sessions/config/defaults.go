package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config, stores func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Global: true,
		Title:  "Sessions",
		About:  "Session storage",
		Settings: []config.Setting{
			{Key: Store, Title: "Store", About: "Where sessions are saved", Default: "sqlite", Options: stores},
		},
	}})
}
