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
		About:  "Search and page reading built for agents, free with limits",
		Order:  30,
		Settings: []config.Setting{
			{Key: APIKey, Title: "API key", About: "Your Tavily API key.",
				Help: "Tavily works without a key once you choose it, and a key raises the limits. Get one at app.tavily.com.", Default: "",
				Env: []string{"TAVILY_API_KEY"}, Secret: true},
			{Key: BaseURL, Title: "Base URL", About: "The address of the Tavily API.", Help: "Change the address only to go through a proxy.", Default: DefaultBaseURL,
				Zero: "The standard address", Env: []string{"TAVILY_BASE_URL"}, Advanced: true},
		},
	}})
}
