package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Agent desktop",
		About: "A hidden desktop the agent can click around in",
		Group: config.GroupAbilities,
		Order: 50,
		Settings: []config.Setting{
			{Key: Placement, Title: "Location", About: "Where the agent desktop runs.", Default: Auto, Options: config.Options(
				config.Option{Value: Auto, Title: "Automatic", About: "Decided by where the agent's commands run"},
				config.Option{Value: Terminal, Title: "With commands", About: "In the same sandbox as the agent's commands"},
				config.Option{Value: Gateway, Title: "This computer", About: "On this computer, even when commands run in a sandbox"},
			)},
			{Key: AutoStart, Title: "Start when needed", About: "Start the agent desktop by itself when a tool needs a screen.",
				Help: "Only on a Linux computer with no display of its own and the needed packages installed.", Default: false},
			{Key: Geometry, Title: "Screen size", About: "The width and height of the desktop screen.", Help: "Written as width x height, such as 1440x900.",
				Default: DefaultGeometry, Zero: DefaultGeometry},
			{Key: IdleStop, Title: "Stop when idle", About: "How long the agent desktop may go unused before it stops.", Default: DefaultIdleStop * time.Minute,
				Unit: time.Minute, Zero: "Never", Advanced: true},
			{Key: MinFreeMemory, Title: "Memory needed", About: "The free memory that the agent desktop needs before it starts.",
				Help: "With less free memory than this, the desktop does not start. " +
					"A little above this amount, the desktop starts with a warning.", Default: DefaultMinFreeMemory, Zero: "No check", Counts: "MB",
				Least: config.Num(0), Advanced: true},
		},
	}})
}
