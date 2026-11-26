package config

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Global: true,
		Title:  "App",
		About:  "Colors and keyboard",
		Settings: []config.Setting{
			{Key: Skin, Title: "Skin", About: "Color theme", Default: theme.DefaultName, Options: skins},
			{Key: Option, Title: "Option as alt", About: "Treat Option as Alt in a Mac terminal", Default: false},
			{Key: Keys, Title: "Keys", About: "Shortcuts changed from their defaults", Default: map[string]any{}, Internal: true},
		},
	}})
}

func skins() []config.Option {
	var options []config.Option
	for _, skin := range theme.All() {
		options = append(options, config.Option{Value: skin.Name, Title: skin.Name, About: skin.About})
	}
	return options
}
