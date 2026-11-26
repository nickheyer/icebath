package config

import (
	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "Terminal",
		About:  "Agent shell commands and terminal windows",
		Settings: []config.Setting{
			{Key: Timeout, Title: "Timeout", About: "Seconds an agent command can run before it's stopped", Default: 180},
			{Key: EnvPassthrough, Title: "Passed variables", About: "Variables ending in _API_KEY or _AUTH_TOKEN that agent commands may still see", Default: []string{}},
			{Key: Shell, Title: "Shell", About: "Command a terminal window runs; empty uses your login shell", Default: ""},
			{Key: Scrollback, Title: "Scrollback", About: "Lines a terminal window keeps above the screen", Default: cards.DefaultScrollback},
		},
	}})
}
