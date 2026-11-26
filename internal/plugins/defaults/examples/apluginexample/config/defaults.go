package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Example plugin",
		About: "Example of how a plugin is built",
		Settings: []config.Setting{
			{Key: Greeting, Title: "Greeting", About: "Text the example plugin shows", Default: "hello"},
		},
	}})
}
