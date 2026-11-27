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
		Order:  20,
		Settings: []config.Setting{
			{Key: MaxTokens, Title: "Max output", About: "The most tokens one reply may take when the request sets no limit.", Default: 0,
				Zero: "The model's limit", Counts: "tokens", Advanced: true},
			{Key: Fallbacks, Title: "Use fallback model", About: "Let the Anthropic API answer a refused request with its own fallback model.",
				Default: true, Advanced: true},
			{Key: BedrockBaseURL, Title: "Claude on Bedrock address", About: "The address Claude on Bedrock is reached at, such as a VPC endpoint.",
				Default: "", Zero: "The region's address", Env: []string{BedrockBaseURLEnv}, Advanced: true},
		},
	}})
}
