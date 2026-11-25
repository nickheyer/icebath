package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "actual"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv      = "ACTUAL_API_KEY"
	BaseURLEnv     = "ACTUAL_BASE_URL"
	DefaultBaseURL = "https://api.actual.inc/v1"
)
