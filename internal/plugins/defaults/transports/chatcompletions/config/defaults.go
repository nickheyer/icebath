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
		About:  "Requests to providers that use the OpenAI chat completions API",
		Settings: []config.Setting{
			{Key: ReasoningEcho, Title: "Echo reasoning back", About: "Sends the model's earlier reasoning back to it on each request; only needed for providers that require it",
				Default: false},
		},
	}})
}
