package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.Wake, Title: "Sherpa wake word", About: "The open vocabulary wake engine, which hears any phrase " +
		"on the native speech engine", Settings: []config.Setting{
		{Key: ModelDir, Title: "Sherpa model folder", About: "Folder holding a Sherpa keyword spotting model; empty fetches the bundled one on first use", Default: ""},
	}}})
}
