package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/web"
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
			{Key: Model, Title: "Model", About: "The model that runs the searches.", Help: "Searches run on the xAI account you connected.", Default: DefaultModel,
				Zero: DefaultModel, Picks: providerconfig.ModelOf("xai", web.XAIOAuth)},
			{Key: AllowedDomains, Title: "Only these sites", About: "The only sites that searches may use.",
				Help: "At most five. Cannot be set together with Sites left out.", Default: []string{}, Entry: &config.Entry{Noun: "site"}},
			{Key: ExcludedDomains, Title: "Sites left out", About: "Sites that searches through Grok leave out.",
				Help: "At most five. Cannot be set together with Only these sites.", Default: []string{}, Entry: &config.Entry{Noun: "site"}},
			{Key: Timeout, Title: "Time limit", About: "How long to wait for one search.", Default: 90 * time.Second, Advanced: true},
		},
	}})
}
