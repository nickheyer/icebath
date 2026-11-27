package config

import (
	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: webconfig.Key,
		Title:  "Exa",
		About:  "Search by meaning and page reading, free with limits",
		Order:  20,
		Settings: []config.Setting{
			{Key: APIKey, Title: "API key", About: "Your Exa API key.", Help: "Get one at exa.ai. Without a key, searches are free but slow down under heavy use.",
				Default: "", Env: []string{"EXA_API_KEY"}, Secret: true},
		},
	}})
}
