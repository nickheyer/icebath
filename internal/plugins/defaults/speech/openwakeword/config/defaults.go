package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.Wake, Title: "openWakeWord", About: "openWakeWord, which hears the phrase a model file was " +
		"trained for", Settings: []config.Setting{
		{Key: Model, Title: "Wake model file", About: "Path to the openWakeWord model file trained for your wake phrase; setting it makes auto pick openWakeWord",
			Default: ""},
	}}})
}
