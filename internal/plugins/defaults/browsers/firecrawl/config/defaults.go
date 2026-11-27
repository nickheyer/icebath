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
		About:  "Firecrawl (https://firecrawl.dev) cloud browser backend, on the key of the Firecrawl web backend but its own endpoints",
		Settings: []config.Setting{
			{Key: TTL, Title: "Firecrawl session life", About: "Seconds a Firecrawl browser session lives", Default: 5 * time.Minute,
				Min: config.Bound(time.Second), Env: []string{"FIRECRAWL_BROWSER_TTL", browserconfig.SessionTimeoutEnv}},
		},
	}})
}
