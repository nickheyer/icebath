package config

import (
	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: webconfig.Key,
		Title:  "Exa",
		About:  "Exa's semantic web search and page reading: free and rate limited without a key, unthrottled with one",
		Settings: []config.Setting{
			{Key: APIKey, Title: "Exa key", About: "API key of Exa for AI-native web search and page contents, at https://exa.ai/", Default: "", Env: []string{"EXA_API_KEY"}, Secret: true},
		},
	}})
}
