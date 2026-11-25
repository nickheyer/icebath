package config

import pluginsconfig "github.com/nickheyer/icebath/internal/plugins/config"

const (
	IntlName         = "alibaba-coding-plan"
	IntlKey          = pluginsconfig.Key + "." + IntlName
	IntlBaseURL      = IntlKey + ".base_url"
	IntlAPIKey       = IntlKey + ".api_key"
	IntlDefaultModel = IntlKey + ".default_model"
)

const (
	CNName         = "alibaba-coding-plan-cn"
	CNKey          = pluginsconfig.Key + "." + CNName
	CNBaseURL      = CNKey + ".base_url"
	CNAPIKey       = CNKey + ".api_key"
	CNDefaultModel = CNKey + ".default_model"
)

const (
	CodingPlanAPIKeyEnv    = "ALIBABA_CODING_PLAN_API_KEY"
	DashScopeAPIKeyEnv     = "DASHSCOPE_API_KEY"
	CodingPlanBaseURLEnv   = "ALIBABA_CODING_PLAN_BASE_URL"
	CodingPlanCNAPIKeyEnv  = "ALIBABA_CODING_PLAN_CN_API_KEY"
	CodingPlanCNBaseURLEnv = "ALIBABA_CODING_PLAN_CN_BASE_URL"
)

const (
	IntlModelsURL = "https://coding-intl.dashscope.aliyuncs.com/v1/models"
	CNModelsURL   = "https://coding.dashscope.aliyuncs.com/v1/models"
)
