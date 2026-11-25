package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "zai"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv       = "GLM_API_KEY"
	ZAIAPIKeyEnv    = "ZAI_API_KEY"
	ZDotAIAPIKeyEnv = "Z_AI_API_KEY"
	BaseURLEnv      = "GLM_BASE_URL"
	ModelsURL       = "https://api.z.ai/api/paas/v4/models"
)
