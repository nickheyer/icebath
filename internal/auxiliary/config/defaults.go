package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config, tasks config.Registrant) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Helper models",
		About: "Models for side jobs such as naming sessions",
		Group: config.GroupModels,
		Order: 30,
		Settings: []config.Setting{
			{Key: OpenRouterModel, Title: "OpenRouter model", About: "The OpenRouter model that side jobs use when they name none.", Default: "",
				Zero: "Built-in free model"},
			{Key: FreeOnly, Title: "Free models only", About: "Use only free OpenRouter models for side jobs.", Default: false},
			{Key: TransientRetries, Title: "Retries", About: "How many times a side job is tried again after a brief network error.", Default: 2,
				Counts: "retries", Least: config.Num(0), Most: config.Num(6), Advanced: true},
		},
	}})
	cfg.Register(tasks)
}
