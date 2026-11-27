package config

import (
	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "Verification",
		About:  "Whether the agent checks the code it changed before it finishes",
		Settings: []config.Setting{
			{Key: OnStop, Title: "Verify before stop", About: "Asks the agent to check its edits before it finishes", Default: Off, Options: config.Options(
				config.Option{Value: Auto, Title: "Auto", About: "Check edits everywhere but in chat apps"},
				config.Option{Value: On, Title: "Always", About: "Always check edits before finishing"},
				config.Option{Value: Off, Title: "Never", About: "Never ask for a check"},
			)},
			{Key: Guidance, Title: "Verify advice", About: "Adds how-to-verify advice to the reminders sent before stopping", Default: true},
			{Key: MaxNudges, Title: "Verify reminders", About: "Most times per turn the agent is told to verify its work; 0 turns the reminders off", Default: 3},
		},
	}})
}
