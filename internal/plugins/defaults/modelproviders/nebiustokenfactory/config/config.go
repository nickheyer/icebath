package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "nebius-token-factory"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv             = "NEBIUS_API_KEY"
	TokenFactoryAPIKeyEnv = "NEBIUS_TOKEN_FACTORY_API_KEY"
	BaseURLEnv            = "NEBIUS_BASE_URL"
	Endpoint              = "https://api.tokenfactory.nebius.com/v1"
	ModelsURL             = "https://api.tokenfactory.nebius.com/v1/models?verbose=true"
)
