package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "lmstudio"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv  = "LM_API_KEY"
	BaseURLEnv = "LM_BASE_URL"
)
