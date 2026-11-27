package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Bot Desktop",
		About: "The agent's own headless screen, a TigerVNC and Xfce desktop per profile that computer use and the browser drive and a person can " +
			"take over",
		Settings: []config.Setting{
			{Key: AutoStart, Title: "Auto start desktop", About: "Start a hidden desktop on its own when a tool needs a screen; only applies on a Linux " +
				"machine with no display and the needed packages installed", Default: false},
			{Key: Geometry, Title: "Desktop size", About: "Screen size of the agent's own desktop, as width x height", Default: DefaultGeometry},
			{Key: IdleStop, Title: "Desktop idle stop", About: "Minutes without use after which the agent's desktop stops; zero or less keeps it running",
				Default: DefaultIdleStop * time.Minute, Unit: time.Minute},
			{Key: MinFreeMemory, Title: "Min free memory", About: "Refuse to start a desktop when free memory is below this many MB; zero turns the " +
				"check off", Default: DefaultMinFreeMemory},
			{Key: Placement, Title: "Desktop placement", About: "Where the agent's desktop runs", Default: Auto, Options: config.Options(
				config.Option{Value: Auto, Title: "Automatic", About: "Pick based on where commands run."},
				config.Option{Value: Terminal, Title: "With commands", About: "Run inside the same place as commands."},
				config.Option{Value: Gateway, Title: "This computer", About: "Run on the machine running the app."},
			)},
		},
	}})
}
