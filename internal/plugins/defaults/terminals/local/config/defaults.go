package config

import (
	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: terminalconfig.Key,
		Title:  "This machine",
		About:  "Commands run directly on this computer",
		Settings: []config.Setting{
			{Key: Persistent, Title: "Persistent local shell", About: "Keep the variables a command exports for the commands after it; off starts each command fresh", Default: true, Env: []string{"TERMINAL_LOCAL_PERSISTENT"}},
			{Key: InitFiles, Title: "Shell init files", About: "Files sourced before commands run, in place of your usual ones; empty falls back to the profile and bashrc files", Default: []string{}},
			{Key: SourceBashrc, Title: "Load shell profile", About: "Load your usual shell startup files before running commands; ignored when shell init files are set", Default: true},
		},
	}})
}
