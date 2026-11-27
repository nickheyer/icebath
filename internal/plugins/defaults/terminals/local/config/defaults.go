package config

import (
	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: terminalconfig.Key,
		Title:  "This computer",
		About:  "Commands that run right on this computer",
		Order:  10,
		Settings: []config.Setting{
			{Key: Persistent, Title: "Keep exported variables", About: "Keep the variables that a command exports for the commands after it.",
				Help: "When off, each command starts fresh.", Default: true, Env: []string{"TERMINAL_LOCAL_PERSISTENT"}},
			{Key: SourceBashrc, Title: "Load shell profile", About: "Load your usual shell startup files before commands run.",
				Help: "Has no effect while Startup files holds any file.", Default: true},
			{Key: InitFiles, Title: "Startup files", About: "Files loaded before commands run, in place of your usual ones.", Default: []string{},
				Zero: "Usual startup files"},
		},
	}})
}
