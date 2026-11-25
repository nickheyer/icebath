package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "gmi"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv  = "GMI_API_KEY"
	BaseURLEnv = "GMI_BASE_URL"
)
