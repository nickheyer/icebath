package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.Wake, Title: "Porcupine wake word", About: "Picovoice Porcupine, which hears its built in " +
		"keywords or a keyword file; needs PORCUPINE_ACCESS_KEY", Settings: []config.Setting{
		{Key: Keyword, Title: "Porcupine keyword", About: "Built in keyword Porcupine listens for, such as jarvis or computer, or the path of a .ppn keyword file",
			Default: DefaultKeyword},
	}}})
}
