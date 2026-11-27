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
		Title:  "Terminal",
		About:  "The agent's shell commands and background processes, and terminal windows",
		Settings: []config.Setting{
			{Key: MaxForeground, Title: "Timeout ceiling", About: "Longest timeout in seconds one foreground command may ask for; a longer one runs the command in the background instead", Default: 10 * time.Minute, Env: []string{"TERMINAL_MAX_FOREGROUND_TIMEOUT"}},
			{Key: WaitNotify, Title: "Notify on finish", About: "Tell the agent when a background process it keeps waiting on with no new output ends, instead of letting it wait", Default: true, Env: []string{"TERMINAL_PROCESS_WAIT_AUTO_NOTIFY"}},
			{Key: EnvironmentHint, Title: "Environment note", About: "Extra text describing your machine, added to the agent's prompt where it is told where its commands run", Default: ""},
			{Key: Shell, Title: "Shell", About: "Command a terminal window runs; empty uses your login shell", Default: ""},
			{Key: Scrollback, Title: "Scrollback", About: "Lines a terminal window keeps above the screen", Default: cards.DefaultScrollback},
		},
	}})
}
