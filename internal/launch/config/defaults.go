package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Startup and exit",
		About: "How long icebath waits when it shuts down",
		Group: config.GroupApp,
		Order: 60,
		Settings: []config.Setting{
			{Key: SafeMode, Title: "Safe mode", About: "Start sessions with none of your customizations.",
				Help: "The settings file, project rules, memory, MCP servers, fallback models and hooks are all left out.", Default: false,
				Env: []string{"ICEBATH_SAFE_MODE"}, Internal: true},
			{Key: ExitWatchdog, Title: "Exit time limit", About: "How long icebath may take to clean up when it exits before it is stopped.",
				Help: "A stop signal doubles this time.", Default: 30 * time.Second, Zero: "No limit", Env: []string{"ICEBATH_EXIT_WATCHDOG_S"},
				Min: config.Bound(0), Advanced: true},
			{Key: SignalGrace, Title: "Stop signal wait", About: "How long a single-question run waits after a stop signal before it ends.",
				Default: 1500 * time.Millisecond, Zero: "Right away", Env: []string{"ICEBATH_SIGTERM_GRACE"}, Min: config.Bound(0), Advanced: true},
		},
	}})
}
