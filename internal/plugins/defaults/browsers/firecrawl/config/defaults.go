package config

import (
	"time"

	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: browserconfig.Key,
		Title:  "Firecrawl browser",
		About:  "Cloud browsers that use your Firecrawl API key",
		Order:  30,
		Settings: []config.Setting{
			{Key: TTL, Title: "Session length", About: "How long one Firecrawl browser session lasts.",
				Help: "Sessions use the API key set in Web search › Firecrawl.", Default: 5 * time.Minute, Min: config.Bound(time.Second),
				Env: []string{"FIRECRAWL_BROWSER_TTL", browserconfig.SessionTimeoutEnv}, Advanced: true},
		},
	}})
}
