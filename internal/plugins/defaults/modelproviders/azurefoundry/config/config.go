package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "azure-foundry"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv  = "AZURE_FOUNDRY_API_KEY"
	BaseURLEnv = "AZURE_FOUNDRY_BASE_URL"
)
