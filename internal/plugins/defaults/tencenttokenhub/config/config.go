package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	TokenHubName         = "tencent-tokenhub"
	TokenHubKey          = pluginsconfig.Key + "." + TokenHubName
	TokenHubBaseURL      = TokenHubKey + ".base_url"
	TokenHubAPIKey       = TokenHubKey + ".api_key"
	TokenHubDefaultModel = TokenHubKey + ".default_model"
)

const (
	TokenPlanName         = "tencent-tokenplan"
	TokenPlanKey          = pluginsconfig.Key + "." + TokenPlanName
	TokenPlanBaseURL      = TokenPlanKey + ".base_url"
	TokenPlanAPIKey       = TokenPlanKey + ".api_key"
	TokenPlanDefaultModel = TokenPlanKey + ".default_model"
)

const (
	TokenHubAPIKeyEnv   = "TOKENHUB_API_KEY"
	TokenHubBaseURLEnv  = "TOKENHUB_BASE_URL"
	TokenPlanAPIKeyEnv  = "TOKENPLAN_API_KEY"
	TokenPlanBaseURLEnv = "TOKENPLAN_BASE_URL"
)

const TokenPlanModelsURL = "https://api.lkeap.cloud.tencent.com/plan/anthropic/models"
