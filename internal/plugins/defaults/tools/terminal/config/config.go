package config

import (
	appconfig "github.com/nickheyer/icebath/internal/app/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

const (
	Key = toolsconfig.Key + ".terminal"
	// Longest timeout a foreground command may ask for, past which it runs in the background
	MaxForeground = Key + ".max_foreground_timeout"
	// A background process waited on with no new output gets its completion notice turned on
	WaitNotify = Key + ".process_wait_auto_notify"
	// Text of the user's own describing their machine, added to where the agent is told its commands run
	EnvironmentHint = Key + ".environment_hint"
)

const (
	// Terminal windows a person opens
	Windows = appconfig.Key + ".terminal"
	// Command a terminal window runs, and the lines it keeps above the screen
	Shell      = Windows + ".shell"
	Scrollback = Windows + ".scrollback"
)

// Kind of the picker of a shell: the shells this computer lists
const PickShell = "shell"
