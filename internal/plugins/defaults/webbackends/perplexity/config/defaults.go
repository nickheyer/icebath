package config

import (
	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: webconfig.Key,
		Title:  "Perplexity",
		About:  "Perplexity's Search API: ranked web results, and the parts of pages relevant to them",
		Settings: []config.Setting{
			{Key: APIKey, Title: "Perplexity key", About: "Perplexity API key for the Search API web backend (ranked results and query-relevant page snippets), from https://www.perplexity.ai/account/api", Default: "", Env: []string{"PERPLEXITY_API_KEY"}, Secret: true},
			{Key: BaseURL, Title: "Perplexity address", About: "Address of the Perplexity API the calls go to", Default: DefaultBaseURL, Env: []string{"PERPLEXITY_BASE_URL"}},
		},
	}})
}
