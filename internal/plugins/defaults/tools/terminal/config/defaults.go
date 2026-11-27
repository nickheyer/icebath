package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "Terminal tool",
		About:  "The agent's commands, background processes and shell windows",
		Order:  30,
		Settings: []config.Setting{
			{Key: EnvironmentHint, Title: "Note about this computer", About: "Text about your computer that the agent reads with where commands run.",
				Help: "Such as the programs installed or what the computer is for.", Default: "", Apply: config.NewSessions},
			{Key: WaitNotify, Title: "Notify instead of wait", About: "Tell the agent when a quiet background process ends, so the agent stops waiting.",
				Default: true, Env: []string{"TERMINAL_PROCESS_WAIT_AUTO_NOTIFY"}},
			{Key: Shell, Title: "Shell", About: "The shell that a terminal window opened with /terminal runs.", Default: "", Zero: "Your usual shell"},
			{Key: Scrollback, Title: "Scrollback", About: "How many lines a terminal window keeps above the screen.", Default: cards.DefaultScrollback,
				Counts: "lines", Least: config.Num(0)},
			{Key: MaxForeground, Title: "Longest wait", About: "The longest time limit that the agent may give a command that it waits for.",
				Help: "A command asked to run longer goes to the background instead.", Default: 10 * time.Minute,
				Env: []string{"TERMINAL_MAX_FOREGROUND_TIMEOUT"}, Advanced: true},
		},
	}})
}
