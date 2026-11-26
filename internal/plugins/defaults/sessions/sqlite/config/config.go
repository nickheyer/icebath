package config

import sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"

const (
	Key  = sessionsconfig.Key + ".sqlite"
	Path = Key + ".path"
)
