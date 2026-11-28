package config

import (
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Global: true,
		Title:  "Appearance",
		About:  "Colors and keyboard",
		Into:   config.GeneralKey,
		Settings: []config.Setting{
			{Key: Skin, Title: "Color theme", About: "Which colors and borders this app draws with.", Default: theme.DefaultName, Options: skins,
				Slash: "skin"},
			{Key: Option, Title: "Option as alt", About: "Use the Option key of a Mac as Alt in shortcuts.",
				Help: "Turn this on when alt shortcuts do nothing in your Mac terminal.", Default: false, Slash: "keys"},
			{Key: Keys, Title: "Keyboard shortcuts", About: "Shortcuts you changed from their defaults.", Default: map[string]any{}, Internal: true,
				Slash: "keys",
				Entry: &config.Entry{Noun: "shortcut", Name: "Action", Fields: []config.Setting{
					{Title: "Keys", About: "The keys that run the action.", Default: []string{}},
				}}},
		},
	}})
}

// Color themes, each titled by its name with a capital first letter
func skins() []config.Option {
	var options []config.Option
	for _, skin := range theme.All() {
		options = append(options, config.Option{Value: skin.Name, Title: strings.ToUpper(skin.Name[:1]) + skin.Name[1:], About: skin.About})
	}
	return options
}
