package config

import (
	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: browserconfig.Key,
		Title:  "Browser Use",
		About:  "Cloud browsers run by Browser Use",
		Order:  10,
		Settings: []config.Setting{
			{Key: APIKey, Title: "API key", About: "Your Browser Use API key.",
				Help: "Get one at browser-use.com. With no browser service chosen, this key opens pages in Browser Use.", Default: "",
				Env: []string{APIKeyEnv}, Secret: true},
		},
	}})
}
