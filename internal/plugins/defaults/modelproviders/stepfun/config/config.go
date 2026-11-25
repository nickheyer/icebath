package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	Name         = "stepfun"
	Key          = pluginsconfig.Key + "." + Name
	BaseURL      = Key + ".base_url"
	APIKey       = Key + ".api_key"
	DefaultModel = Key + ".default_model"
)

const (
	APIKeyEnv  = "STEPFUN_API_KEY"
	BaseURLEnv = "STEPFUN_BASE_URL"
	ModelsURL  = "https://api.stepfun.ai/step_plan/v1/models"
)
