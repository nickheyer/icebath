package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Key            = pluginsconfig.Key + ".terminal"
	Timeout        = Key + ".timeout"
	EnvPassthrough = Key + ".env_passthrough"
)
