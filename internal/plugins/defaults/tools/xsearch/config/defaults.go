package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "X (Twitter) Search",
		About:  "Read-only search of public X posts through xAI's built-in x_search tool, with the xAI API key or Grok sign in connected in /providers",
		Settings: []config.Setting{
			{Key: Model, Title: "X search model", About: "Model that runs X searches", Default: DefaultModel},
			{Key: ReasoningEffort, Title: "X search effort", About: "How hard the model thinks while searching X", Default: "", Options: config.Options(
				config.Option{Title: "Default", About: "The model's default"},
				config.Option{Value: "low", Title: "Low", About: "Fastest, least thinking"},
				config.Option{Value: "medium", Title: "Medium", About: "Balanced"},
				config.Option{Value: "high", Title: "High", About: "More thinking"},
				config.Option{Value: "xhigh", Title: "Extra high", About: "Most thinking"},
			)},
			{Key: Retries, Title: "X search retries", About: "How many times a failed X search is retried", Default: 2},
			{Key: TimeoutSeconds, Title: "X search timeout", About: "Longest to wait for an X search, in seconds, never less than 30", Default: 3 * time.Minute,
				Min: config.Bound(30 * time.Second)},
		},
	}})
}
