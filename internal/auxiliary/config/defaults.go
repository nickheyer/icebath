package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config, tasks config.Registrant) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Helper models",
		About: "Models side jobs run on, such as image analysis, and where they fall back to",
		Settings: []config.Setting{
			{Key: TransientRetries, Title: "Helper retries", About: "Retries of a helper call after a brief network error, 0 to 6", Default: 2},
			{Key: OpenRouterModel, Title: "Fallback free model", About: "Free OpenRouter model a helper falls back to; empty uses a built-in free model", Default: ""},
			{Key: FreeOnly, Title: "Free models only", About: "Only fall back to free OpenRouter models", Default: false},
		},
	}})
	cfg.Register(tasks)
}
