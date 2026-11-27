package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: webconfig.Key,
		Title:  "xAI web search",
		About:  "Web search through Grok with your xAI account",
		Order:  100,
		Settings: []config.Setting{
			{Key: Model, Title: "Model", About: "The model that runs the searches.", Help: "Connect your xAI account in /providers.", Default: DefaultModel,
				Zero: DefaultModel},
			{Key: AllowedDomains, Title: "Only these sites", About: "The only sites that searches may use.",
				Help: "At most five. Cannot be set together with Blocked sites.", Default: []string{}},
			{Key: ExcludedDomains, Title: "Blocked sites", About: "Sites that searches leave out.",
				Help: "At most five. Cannot be set together with Only these sites.", Default: []string{}},
			{Key: Timeout, Title: "Time limit", About: "How long to wait for one search.", Default: 90 * time.Second, Advanced: true},
		},
	}})
}
