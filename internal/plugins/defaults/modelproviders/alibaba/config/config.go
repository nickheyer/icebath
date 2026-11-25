package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	IntlName         = "alibaba"
	IntlKey          = pluginsconfig.Key + "." + IntlName
	IntlBaseURL      = IntlKey + ".base_url"
	IntlAPIKey       = IntlKey + ".api_key"
	IntlDefaultModel = IntlKey + ".default_model"
)

const (
	CNName         = "alibaba-cn"
	CNKey          = pluginsconfig.Key + "." + CNName
	CNBaseURL      = CNKey + ".base_url"
	CNAPIKey       = CNKey + ".api_key"
	CNDefaultModel = CNKey + ".default_model"
)

const (
	TokenPlanName         = "alibaba-token-plan"
	TokenPlanKey          = pluginsconfig.Key + "." + TokenPlanName
	TokenPlanBaseURL      = TokenPlanKey + ".base_url"
	TokenPlanAPIKey       = TokenPlanKey + ".api_key"
	TokenPlanDefaultModel = TokenPlanKey + ".default_model"
)

const (
	TokenPlanCNName         = "alibaba-token-plan-cn"
	TokenPlanCNKey          = pluginsconfig.Key + "." + TokenPlanCNName
	TokenPlanCNBaseURL      = TokenPlanCNKey + ".base_url"
	TokenPlanCNAPIKey       = TokenPlanCNKey + ".api_key"
	TokenPlanCNDefaultModel = TokenPlanCNKey + ".default_model"
)

const (
	DashScopeAPIKeyEnv    = "DASHSCOPE_API_KEY"
	DashScopeBaseURLEnv   = "DASHSCOPE_BASE_URL"
	DashScopeCNBaseURLEnv = "DASHSCOPE_CN_BASE_URL"
	TokenPlanAPIKeyEnv    = "ALIBABA_TOKEN_PLAN_API_KEY"
	TokenPlanBaseURLEnv   = "ALIBABA_TOKEN_PLAN_BASE_URL"
	TokenPlanCNAPIKeyEnv  = "ALIBABA_TOKEN_PLAN_CN_API_KEY"
	TokenPlanCNBaseURLEnv = "ALIBABA_TOKEN_PLAN_CN_BASE_URL"
)

const (
	IntlModelsURL        = "https://dashscope-intl.aliyuncs.com/compatible-mode/v1/models"
	CNModelsURL          = "https://dashscope.aliyuncs.com/compatible-mode/v1/models"
	TokenPlanModelsURL   = "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1/models"
	TokenPlanCNModelsURL = "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/models"
)
