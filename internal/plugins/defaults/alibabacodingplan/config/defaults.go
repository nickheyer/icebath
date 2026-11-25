package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(IntlBaseURL, "https://coding-intl.dashscope.aliyuncs.com/v1")
	cfg.SetDefault(IntlDefaultModel, "")
	cfg.MustBindEnv(IntlBaseURL, CodingPlanBaseURLEnv)
	cfg.MustBindEnv(IntlAPIKey, CodingPlanAPIKeyEnv, DashScopeAPIKeyEnv)

	cfg.SetDefault(CNBaseURL, "https://coding.dashscope.aliyuncs.com/v1")
	cfg.SetDefault(CNDefaultModel, "")
	cfg.MustBindEnv(CNBaseURL, CodingPlanCNBaseURLEnv)
	cfg.MustBindEnv(CNAPIKey, CodingPlanCNAPIKeyEnv, CodingPlanAPIKeyEnv, DashScopeAPIKeyEnv)
}
