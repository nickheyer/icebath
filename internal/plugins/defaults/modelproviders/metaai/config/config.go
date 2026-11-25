package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "meta-ai"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv          = "MODEL_API_KEY"
	MetaAPIKeyEnv      = "META_API_KEY"
	MetaModelAPIKeyEnv = "META_MODEL_API_KEY"
	BaseURLEnv         = "META_BASE_URL"
)
