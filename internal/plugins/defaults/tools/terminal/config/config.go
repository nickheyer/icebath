package config

import toolsconfig "github.com/nickheyer/icebath/internal/tools/config"

const (
	Key            = toolsconfig.Key + ".terminal"
	Timeout        = Key + ".timeout"
	EnvPassthrough = Key + ".env_passthrough"
	Shell          = Key + ".shell"
	Scrollback     = Key + ".scrollback"
)
