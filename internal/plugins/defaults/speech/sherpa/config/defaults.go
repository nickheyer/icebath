package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.Wake, Title: "Sherpa", About: "Hears any phrase you choose, on this computer",
		Order: 10, Settings: []config.Setting{
			// Read by the listener on the computer the app runs on, which looks for it there as it is set, so the settings look for nothing
			{Key: ModelDir, Title: "Model folder", About: "The folder that holds a Sherpa keyword model.", Help: "Empty means the bundled model, which installing Sherpa downloads.",
				Default: "", Zero: "The bundled model", Path: &config.Path{Kind: config.PathFolder}, Advanced: true},
		}}})
}
