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
		About:  "Parallel's objective-tuned web search and page reading: free and rate limited without a key, unthrottled with one",
		Settings: []config.Setting{
			{Key: APIKey, Title: "Parallel key", About: "API key of Parallel for AI-native web search and extract, at https://parallel.ai/", Default: "", Env: []string{"PARALLEL_API_KEY"}, Secret: true},
			{Key: BaseURL, Title: "Parallel address", About: "Address of the Parallel API the key is used with, such as a proxy in front of it", Default: DefaultBaseURL, Env: []string{"PARALLEL_BASE_URL"}},
			{Key: SearchMode, Title: "Search mode", About: "How Parallel answers searches made with the key", Default: Agentic, Env: []string{"PARALLEL_SEARCH_MODE"}, Options: config.Options(
				config.Option{Value: Agentic, Title: "Agentic", About: "Concise, token-efficient results for an agent that searches again"},
				config.Option{Value: OneShot, Title: "One-shot", About: "Fuller results and longer excerpts that answer in one search"},
				config.Option{Value: Fast, Title: "Fast", About: "Results at the lowest latency"},
			)},
			{Key: Headers, Title: "Extra headers", About: "Headers sent with every call to the Parallel API, each as Name: value. Its variable holds one a line", Default: []string{}, Env: []string{"PARALLEL_CUSTOM_HEADERS"}},
		},
	}})
}
