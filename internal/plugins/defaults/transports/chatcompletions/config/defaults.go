package config

import (
	"github.com/nickheyer/icebath/internal/config"
	transportconfig "github.com/nickheyer/icebath/internal/transport/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: transportconfig.Key,
		Title:  "Chat completions API",
		About:  "Requests to providers that use OpenAI chat completions",
		Order:  30,
		Settings: []config.Setting{
			{Key: ReasoningEcho, Title: "Send reasoning back", About: "Send the earlier reasoning of the model back with each request.",
				Help: "Only some providers need this.", Default: false, Advanced: true},
		},
	}})
}
