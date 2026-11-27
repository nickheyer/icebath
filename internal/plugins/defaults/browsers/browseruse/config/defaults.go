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
		About:  "Browser Use (https://browser-use.com) cloud browser backend",
		Settings: []config.Setting{
			{Key: APIKey, Title: "Browser Use key", About: "Browser Use API key; set with no browser service chosen, pages open in Browser Use's cloud", Default: "",
				Env: []string{APIKeyEnv}, Secret: true},
		},
	}})
}
