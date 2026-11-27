package config

import (
	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: webconfig.Key,
		Title:  "Brave Search",
		About:  "Web search on Brave's free plan, 2,000 searches a month",
		Order:  40,
		Settings: []config.Setting{
			{Key: APIKey, Title: "API key", About: "Your Brave Search API key.", Help: "Free at brave.com/search/api.", Default: "",
				Env: []string{"BRAVE_SEARCH_API_KEY"}, Secret: true},
		},
	}})
}
