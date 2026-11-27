package config

import (
	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: webconfig.Key,
		Title:  "Tavily",
		About:  "Tavily's AI-native web search and page reading, keyless when chosen and keyed for higher limits",
		Settings: []config.Setting{
			{Key: APIKey, Title: "Tavily key", About: "Tavily API key for web search and extract, from https://app.tavily.com/home. Optional: keyless works when Tavily is chosen", Default: "", Env: []string{"TAVILY_API_KEY"}, Secret: true},
			{Key: BaseURL, Title: "Tavily address", About: "Address of the Tavily API the calls go to", Default: DefaultBaseURL, Env: []string{"TAVILY_BASE_URL"}},
		},
	}})
}
