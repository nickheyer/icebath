package config

import (
	"github.com/nickheyer/icebath/internal/config"
	transportconfig "github.com/nickheyer/icebath/internal/transport/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: transportconfig.Key,
		Title:  "Anthropic API",
		About:  "Requests to providers that use the Anthropic messages API",
		Settings: []config.Setting{
			{Key: MaxTokens, Title: "Max output tokens", About: "Max tokens in one reply", Default: 64000},
			{Key: Fallbacks, Title: "Server-side fallback", About: "Let the official API retry a refused request with a fallback model", Default: true},
		},
	}})
}
