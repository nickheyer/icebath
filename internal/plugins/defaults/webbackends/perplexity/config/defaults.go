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
		About:  "Ranked web results with the parts of pages that matter",
		Order:  60,
		Settings: []config.Setting{
			{Key: APIKey, Title: "API key", About: "Your Perplexity API key.", Help: "Get one at perplexity.ai/account/api.", Default: "",
				Env: []string{"PERPLEXITY_API_KEY"}, Secret: true},
			{Key: BaseURL, Title: "Base URL", About: "The address of the Perplexity API.", Help: "Change the address only to go through a proxy.",
				Default: DefaultBaseURL, Zero: "The standard address", Env: []string{"PERPLEXITY_BASE_URL"}, Advanced: true},
		},
	}})
}
