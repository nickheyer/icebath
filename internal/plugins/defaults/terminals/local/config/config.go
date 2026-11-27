package config

import terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"

const (
	Key = terminalconfig.Key + ".local"
	// Exported variables carry from one command to the next
	Persistent = Key + ".persistent"
	// Files sourced before commands run, in place of the usual ones
	InitFiles = Key + ".shell_init_files"
	// Sources the usual profile and bashrc files when no init files are named
	SourceBashrc = Key + ".auto_source_bashrc"
)
