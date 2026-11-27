package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Example plugin",
		About: "Shows how a plugin adds settings of its own",
		Settings: []config.Setting{
			{Key: Greeting, Title: "Greeting", About: "The text the example plugin shows.", Default: "hello"},
		},
	}})
}
