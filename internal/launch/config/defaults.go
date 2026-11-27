package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Launch",
		About: "What a launch of icebath asks of the sessions it runs",
		Settings: []config.Setting{
			{Key: SafeMode, Title: "Safe mode", About: "Sessions leave out every customization: the profile's config, project rules and memory, MCP servers and fallback models",
				Default: false, Env: []string{"ICEBATH_SAFE_MODE"}, Internal: true},
			{Key: ExitWatchdog, Title: "Exit watchdog", About: "Longest the command line's cleanup may take before it is ended anyway; a stop signal doubles it, zero turns it off",
				Default: 30 * time.Second, Env: []string{"ICEBATH_EXIT_WATCHDOG_S"}, Min: config.Bound(0)},
			{Key: SignalGrace, Title: "Stop signal grace", About: "Wait after a stop signal interrupts a one-shot run before it ends; zero ends it at once",
				Default: 1500 * time.Millisecond, Env: []string{"ICEBATH_SIGTERM_GRACE"}, Min: config.Bound(0)},
		},
	}})
}
