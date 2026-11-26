package config

const (
	Name         = "nebius-token-factory"
	Title        = "Nebius Token Factory"
	BaseURL      = Endpoint
	DefaultModel = ""
)

const (
	APIKeyEnv             = "NEBIUS_API_KEY"
	TokenFactoryAPIKeyEnv = "NEBIUS_TOKEN_FACTORY_API_KEY"
	BaseURLEnv            = "NEBIUS_BASE_URL"
	Endpoint              = "https://api.tokenfactory.nebius.com/v1"
	ModelsURL             = "https://api.tokenfactory.nebius.com/v1/models?verbose=true"
)
