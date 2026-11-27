package config

import (
	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: webconfig.Key,
		Title:  "SearXNG",
		About:  "Your own SearXNG instance: free, self-hosted, privacy-respecting metasearch, search only",
		Settings: []config.Setting{
			{Key: URL, Title: "SearXNG URL", About: "URL of your SearXNG instance for free self-hosted web search, such as http://localhost:8080 (https://searxng.github.io/searxng/)", Default: "", Env: []string{"SEARXNG_URL"}},
		},
	}})
}
