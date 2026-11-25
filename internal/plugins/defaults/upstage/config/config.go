package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "upstage"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv  = "UPSTAGE_API_KEY"
	BaseURLEnv = "UPSTAGE_BASE_URL"
	ModelsURL  = "https://api.upstage.ai/v1/models"
)
