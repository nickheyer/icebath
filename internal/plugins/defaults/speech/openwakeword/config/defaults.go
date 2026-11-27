package config

import (
	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{Key: Key, Parent: speechconfig.Wake, Title: "openWakeWord", About: "Hears a phrase that a model file was trained for",
		Order: 20, Settings: []config.Setting{
			{Key: Model, Title: "Model file", About: "The model file trained for your wake phrase.", Help: "Once one is set, Automatic picks openWakeWord.",
				Default: "", Advanced: true},
		}}})
}
