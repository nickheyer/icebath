package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "deepseek"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv  = "DEEPSEEK_API_KEY"
	BaseURLEnv = "DEEPSEEK_BASE_URL"
	ModelsURL  = "https://api.deepseek.com/v1/models"
)
