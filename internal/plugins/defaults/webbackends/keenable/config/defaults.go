package config

import (
	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: webconfig.Key,
		Title:  "Keenable",
		About:  "Fast search and page reading from Keenable's own index",
		Order:  90,
		Settings: []config.Setting{
			{Key: APIKey, Title: "API key", About: "Your Keenable API key.",
				Help: "The free plan works without a key, and a key raises the limits. Get one at keenable.ai.", Default: "",
				Env: []string{APIKeyEnv}, Secret: true},
		},
	}})
}
