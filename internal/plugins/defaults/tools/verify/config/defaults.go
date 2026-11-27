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
		About:  "Whether the agent checks its changes before it finishes",
		Order:  90,
		Settings: []config.Setting{
			{Key: OnStop, Title: "Check before finishing", About: "When the agent is asked to check its edits before it finishes a reply.", Default: Off,
				Options: config.Options(
					config.Option{Value: Auto, Title: "Automatic", About: "Everywhere except in chat apps"},
					config.Option{Value: On, Title: "Always", About: "After every reply that changed code"},
					config.Option{Value: Off, Title: "Never", About: "The agent is never asked to check"},
				)},
			{Key: Guidance, Title: "How-to advice", About: "Add advice on how to check the work to each reminder.", Default: true},
			{Key: MaxNudges, Title: "Reminders per reply", About: "How many times in one reply the agent is reminded to check the work.", Default: 3,
				Zero: "Off", Counts: "reminders", Least: config.Num(0)},
		},
	}})
}
