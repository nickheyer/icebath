package config

import toolsconfig "github.com/nickheyer/icebath/internal/tools/config"

const (
	Key = toolsconfig.Key + ".terminal"
	// Longest timeout a foreground command may ask for, past which it runs in the background
	MaxForeground = Key + ".max_foreground_timeout"
	// A background process waited on with no new output gets its completion notice turned on
	WaitNotify = Key + ".process_wait_auto_notify"
	// Text of the user's own describing their machine, added to where the agent is told its commands run
	EnvironmentHint = Key + ".environment_hint"
	// Command a terminal window runs, and the lines it keeps above the screen
	Shell      = Key + ".shell"
	Scrollback = Key + ".scrollback"
)
