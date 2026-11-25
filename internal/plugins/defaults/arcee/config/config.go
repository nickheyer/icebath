package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "arcee"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv  = "ARCEEAI_API_KEY"
	BaseURLEnv = "ARCEE_BASE_URL"
	ModelsURL  = "https://api.arcee.ai/api/v1/models"
)
