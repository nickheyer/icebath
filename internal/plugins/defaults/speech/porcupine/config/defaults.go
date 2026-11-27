package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.Wake, Title: "Porcupine", About: "Hears a few fixed keywords, with a free Picovoice key",
		Order: 30, Settings: []config.Setting{
			{Key: Keyword, Title: "Keyword", About: "The word Porcupine listens for.",
				Help: "A built-in keyword such as jarvis or computer, or the path of a .ppn keyword file.", Default: DefaultKeyword},
		}}})
}
