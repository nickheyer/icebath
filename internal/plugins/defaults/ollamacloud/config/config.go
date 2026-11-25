package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "ollama-cloud"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv  = "OLLAMA_API_KEY"
	BaseURLEnv = "OLLAMA_BASE_URL"
	ModelsURL  = "https://ollama.com/v1/models"
)
