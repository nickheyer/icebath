package config

import (
	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: webconfig.Key,
		Title:  "Firecrawl",
		About:  "Search and page reading, in Firecrawl's cloud or your own",
		Order:  10,
		Settings: []config.Setting{
			{Key: APIKey, Title: "API key", About: "Your Firecrawl API key.",
				Help: "Get one at firecrawl.dev. With a key, Firecrawl also reads scanned PDFs.", Default: "", Env: []string{APIKeyEnv}, Secret: true},
			{Key: APIURL, Title: "Base URL", About: "The address of a Firecrawl server that you run yourself.", Help: "Such as http://localhost:3002.",
				Default: "", Zero: "Firecrawl's cloud", Env: []string{APIURLEnv}, Advanced: true},
		},
	}})
}
