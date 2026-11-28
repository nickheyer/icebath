package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/internal/web"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "X search",
		About:  "Search public posts on X with your xAI account",
		Order:  120,
		Settings: []config.Setting{
			{Key: Model, Title: "Model", About: "The model that runs searches of X.", Help: "A model of xAI, such as grok-4.5. Searches run on your xAI account.", Default: DefaultModel,
				Zero: DefaultModel, Picks: providerconfig.ModelOf("xai", web.XAIOAuth)},
			{Key: ReasoningEffort, Title: "Reasoning effort", About: "How hard the model thinks while it searches X.", Default: "", Options: config.Options(
				config.Option{Title: "The model's default", About: "As much as the model does by itself"},
				config.Option{Value: "low", Title: "Low", About: "The quickest answers, with the least thinking"},
				config.Option{Value: "medium", Title: "Medium", About: "A balance of speed and thinking"},
				config.Option{Value: "high", Title: "High", About: "More thinking, slower answers"},
				config.Option{Value: "xhigh", Title: "Extra high", About: "The most thinking, the slowest answers"},
			)},
			{Key: Retries, Title: "Retries", About: "How many times a failed search is tried again.", Default: 2, Counts: "retries", Least: config.Num(0),
				Advanced: true},
			{Key: TimeoutSeconds, Title: "Time limit", About: "How long to wait for one search.", Default: 3 * time.Minute, Min: config.Bound(30 * time.Second),
				Advanced: true},
		},
	}})
}
