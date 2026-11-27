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
		About:  "Python scripts the agent runs that call its tools",
		Settings: []config.Setting{
			{Key: Timeout, Title: "Code run limit", About: "Longest one code run may take before it is stopped, in seconds", Default: 5 * time.Minute},
			{Key: MaxToolCalls, Title: "Script tool calls", About: "Most tool calls one code run may make", Default: 50},
			{Key: MaxSessionKernels, Title: "Max code sessions", About: "How many code sessions can stay open at once; at least 1", Default: 4},
			{Key: KernelIdleTimeout, Title: "Kernel idle close", About: "Close an idle code session after this many seconds; never less than 1", Default: 30 * time.Minute,
				Min: config.Bound(time.Second)},
			{Key: Mode, Title: "Code run folder", About: "Where scripts run: your working folder or a scratch one", Default: Project, Options: config.Options(
				config.Option{Value: Project, Title: "Project folder", About: "Scripts run in your working folder."},
				config.Option{Value: Strict, Title: "Scratch folder", About: "Scripts run in a temporary folder."},
			)},
		},
	}})
}
