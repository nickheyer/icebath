package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.Wake, Title: "openWakeWord", About: "Hears a phrase that a model file was trained for",
		Order: 20, Settings: []config.Setting{
			// Read by the listener on the computer the app runs on, which looks for it there as it is set, so the settings look for nothing
			{Key: Model, Title: "Model file", About: "The model file trained for your wake phrase.", Help: "Once one is set, Automatic picks openWakeWord.",
				Default: "", Path: &config.Path{Kind: config.PathFile}, Advanced: true},
		}}})
}
