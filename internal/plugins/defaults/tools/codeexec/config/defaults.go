package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "Code execution",
		About:  "Python scripts that the agent writes to call tools",
		Order:  80,
		Settings: []config.Setting{
			{Key: Mode, Title: "Where scripts run", About: "Which folder the agent's scripts run in.", Default: Project, Options: config.Options(
				config.Option{Value: Project, Title: "Project folder", About: "Your working folder, with its Python environment"},
				config.Option{Value: Strict, Title: "Scratch folder", About: "A temporary folder, apart from your project"},
			)},
			{Key: MaxToolCalls, Title: "Tool calls per script", About: "The most tool calls that one script may make.", Default: 50, Zero: "None allowed",
				Counts: "calls"},
			{Key: MaxSessionKernels, Title: "Open code sessions", About: "How many code sessions may stay open at the same time.", Default: 4,
				Counts: "sessions", Least: config.Num(1)},
			{Key: Timeout, Title: "Script time limit", About: "How long one script may run before it is stopped.", Help: "At least 1 second.",
				Default: 5 * time.Minute, Advanced: true},
			{Key: KernelIdleTimeout, Title: "Close when idle", About: "How long an unused code session stays open.", Default: 30 * time.Minute,
				Min: config.Bound(time.Second), Advanced: true},
		},
	}})
}
