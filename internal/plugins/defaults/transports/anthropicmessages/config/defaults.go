package config

import (
	"github.com/nickheyer/icebath/internal/config"
	transportconfig "github.com/nickheyer/icebath/internal/transport/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: transportconfig.Key,
		Title:  "Anthropic messages API",
		About:  "Requests to providers that use the Anthropic messages API",
		Settings: []config.Setting{
			{Key: MaxTokens, Title: "Max output tokens", About: "Most tokens one reply may take when the request sets none; 0 for the model's own ceiling",
				Default: 0},
			{Key: Fallbacks, Title: "Server-side fallback", About: "Let the Anthropic API answer a refused request with its fallback model", Default: true},
			{Key: BedrockBaseURL, Title: "Claude on Bedrock address", About: "Address Claude on Bedrock is reached at in place of the region's runtime " +
				"endpoint, such as a VPC endpoint; empty for the region's", Default: "", Env: []string{BedrockBaseURLEnv}},
		},
	}})
}
