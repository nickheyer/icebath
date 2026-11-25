package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Key       = pluginsconfig.Key + ".custom"
	Providers = Key + ".providers"
)

const (
	BaseURL      = "base_url"
	APIKey       = "api_key"
	KeyEnv       = "key_env"
	APIMode      = "api_mode"
	DefaultModel = "default_model"
	DisplayName  = "name"
)
