package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "qwen-oauth"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv  = "QWEN_API_KEY"
	BaseURLEnv = "ICEBATH_QWEN_BASE_URL"
)
