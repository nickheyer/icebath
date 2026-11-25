package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "gemini"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	GoogleAPIKeyEnv = "GOOGLE_API_KEY"
	GeminiAPIKeyEnv = "GEMINI_API_KEY"
	BaseURLEnv      = "GEMINI_BASE_URL"
)
