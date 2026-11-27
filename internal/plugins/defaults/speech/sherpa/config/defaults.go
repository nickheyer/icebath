package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.Wake, Title: "Sherpa", About: "Hears any phrase you choose, on this computer",
		Order: 10, Settings: []config.Setting{
			{Key: ModelDir, Title: "Model folder", About: "The folder that holds a Sherpa keyword model.", Help: "The bundled model is downloaded on first use.",
				Default: "", Zero: "The bundled model", Advanced: true},
		}}})
}
