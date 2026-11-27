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
		About:  "Keenable's independent web index for AI apps: fast search and page fetch, free without a key",
		Settings: []config.Setting{
			{Key: APIKey, Title: "Keenable key", About: "Keenable API key for fast independent-index web search and page fetch, from https://keenable.ai. Optional: the keyless free tier works without it", Default: "", Env: []string{APIKeyEnv}, Secret: true},
		},
	}})
}
