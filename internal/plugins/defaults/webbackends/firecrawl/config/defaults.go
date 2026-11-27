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
		About:  "Firecrawl web search and page reading: its cloud with a key or keyless, or a self-hosted instance",
		Settings: []config.Setting{
			{Key: APIKey, Title: "Firecrawl key", About: "Firecrawl API key for web search and scraping, and for reading scanned PDFs, from https://firecrawl.dev/", Default: "", Env: []string{APIKeyEnv}, Secret: true},
			{Key: APIURL, Title: "Firecrawl address", About: "Firecrawl API URL of a self-hosted instance, such as http://localhost:3002. Empty uses Firecrawl's cloud", Default: "", Env: []string{APIURLEnv}},
		},
	}})
}
