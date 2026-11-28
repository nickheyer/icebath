package config

import (
	"time"

	appconfig "github.com/nickheyer/icebath/internal/app/config"
	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "Terminal tool",
		About:  "How the agent's commands wait and what the agent reads of this computer",
		Order:  30,
		Into:   terminalconfig.Key,
		Settings: []config.Setting{
			{Key: WaitNotify, Title: "Notify instead of wait", About: "Tell the agent when a quiet background process ends, so the agent stops waiting.",
				Default: true, Env: []string{"TERMINAL_PROCESS_WAIT_AUTO_NOTIFY"}},
			{Key: MaxForeground, Title: "Longest wait", About: "The longest time limit that the agent may give a command that it waits for.",
				Help: "A command asked to run longer goes to the background instead.", Default: 10 * time.Minute,
				Env: []string{"TERMINAL_MAX_FOREGROUND_TIMEOUT"}},
			{Key: EnvironmentHint, Title: "Note about this computer", About: "Text about your computer that the agent reads with where commands run.",
				Help: "Such as the programs installed or what the computer is for.", Default: "", Apply: config.NewSessions},
		},
	}, {
		Key:    Windows,
		Parent: appconfig.Key,
		Title:  "Terminal windows",
		About:  "The shell windows you open with /terminal",
		Order:  20,
		Into:   config.GeneralKey,
		Settings: []config.Setting{
			{Key: Shell, Title: "Shell", About: "The shell that a terminal window runs.", Help: "The command that starts it, with what it is handed.",
				Default: "", Zero: "Your usual shell", Picks: PickShell},
			{Key: Scrollback, Title: "Scrollback", About: "How many lines a terminal window keeps above the screen.", Default: cards.DefaultScrollback,
				Counts: "lines", Least: config.Num(0)},
		},
	}})
}
