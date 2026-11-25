package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "openrouter"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv  = "OPENROUTER_API_KEY"
	BaseURLEnv = "OPENROUTER_BASE_URL"
	ModelsURL  = "https://openrouter.ai/api/v1/models"
)
