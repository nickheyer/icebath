package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "vertex"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	DefaultModel = Key + ".default_model"
)
