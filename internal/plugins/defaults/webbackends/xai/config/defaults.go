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
		Title:  "xAI Web Search",
		About:  "Grok's web_search tool through the xAI Responses API, with the xAI API key or Grok sign in connected in /providers",
		Settings: []config.Setting{
			{Key: Model, Title: "xAI search model", About: "Model that runs xAI web searches", Default: DefaultModel},
			{Key: Timeout, Title: "xAI search timeout", About: "Longest to wait for an xAI web search, in seconds", Default: 90 * time.Second},
			{Key: AllowedDomains, Title: "xAI allowed sites", About: "Only sites xAI web searches may use, at most five. Cannot be set together with xAI blocked sites", Default: []string{}},
			{Key: ExcludedDomains, Title: "xAI blocked sites", About: "Sites xAI web searches leave out, at most five. Cannot be set together with xAI allowed sites", Default: []string{}},
		},
	}})
}
