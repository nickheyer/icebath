package config

import (
	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: webconfig.Key,
		Title:  "Parallel",
		About:  "Search tuned for agents, and page reading, free with limits",
		Order:  50,
		Settings: []config.Setting{
			{Key: APIKey, Title: "API key", About: "Your Parallel API key.",
				Help: "Get one at parallel.ai. Without a key, searches are free but slow down under heavy use.", Default: "",
				Env: []string{"PARALLEL_API_KEY"}, Secret: true},
			{Key: SearchMode, Title: "Search mode", About: "How Parallel answers searches made with your key.", Default: Agentic,
				Env: []string{"PARALLEL_SEARCH_MODE"}, Options: config.Options(
					config.Option{Value: Agentic, Title: "For agents", About: "Short results for an agent that searches again"},
					config.Option{Value: OneShot, Title: "Complete", About: "Fuller results and longer passages that answer in one search"},
					config.Option{Value: Fast, Title: "Fast", About: "The quickest results"},
				)},
			{Key: BaseURL, Title: "Base URL", About: "The address of the Parallel API.", Help: "Change the address only to go through a proxy.", Default: DefaultBaseURL,
				Zero: "The standard address", Env: []string{"PARALLEL_BASE_URL"}, Advanced: true},
			{Key: Headers, Title: "Extra headers", About: "Headers sent with every request to Parallel.",
				Help: "Each written as Name: value. In the environment variable, one header per line.", Default: []string{},
				Env: []string{"PARALLEL_CUSTOM_HEADERS"}, Advanced: true},
		},
	}})
}
