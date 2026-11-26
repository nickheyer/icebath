package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config, tools func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Tools",
		About: "Tool access and output limits",
		Settings: []config.Setting{
			{Key: Disabled, Title: "Disabled tools", About: "Tools the agent can't use", Default: []string{}, Options: tools},
			{Key: OutputMaxBytes, Title: "Max output size", About: "Max bytes of tool output sent to the model", Default: 50000},
			{Key: OutputMaxLineLength, Title: "Max line length", About: "Characters per line of tool output before the rest is cut", Default: 2000},
		},
	}})
}
