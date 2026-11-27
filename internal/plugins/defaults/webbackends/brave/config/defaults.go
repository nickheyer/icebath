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
		About:  "Brave's Data for Search API on its free tier: 2,000 searches a month, search only",
		Settings: []config.Setting{
			{Key: APIKey, Title: "Brave Search key", About: "Subscription token of the Brave Search API, free at https://brave.com/search/api/", Default: "", Env: []string{"BRAVE_SEARCH_API_KEY"}, Secret: true},
		},
	}})
}
