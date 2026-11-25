package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(IntlBaseURL, "https://dashscope-intl.aliyuncs.com/compatible-mode/v1")
	cfg.SetDefault(IntlDefaultModel, "")
	cfg.MustBindEnv(IntlBaseURL, DashScopeBaseURLEnv)
	cfg.MustBindEnv(IntlAPIKey, DashScopeAPIKeyEnv)

	cfg.SetDefault(CNBaseURL, "https://dashscope.aliyuncs.com/compatible-mode/v1")
	cfg.SetDefault(CNDefaultModel, "")
	cfg.MustBindEnv(CNBaseURL, DashScopeCNBaseURLEnv)
	cfg.MustBindEnv(CNAPIKey, DashScopeAPIKeyEnv)

	cfg.SetDefault(TokenPlanBaseURL, "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1")
	cfg.SetDefault(TokenPlanDefaultModel, "")
	cfg.MustBindEnv(TokenPlanBaseURL, TokenPlanBaseURLEnv)
	cfg.MustBindEnv(TokenPlanAPIKey, TokenPlanAPIKeyEnv)

	cfg.SetDefault(TokenPlanCNBaseURL, "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1")
	cfg.SetDefault(TokenPlanCNDefaultModel, "")
	cfg.MustBindEnv(TokenPlanCNBaseURL, TokenPlanCNBaseURLEnv)
	cfg.MustBindEnv(TokenPlanCNAPIKey, TokenPlanCNAPIKeyEnv, TokenPlanAPIKeyEnv)
}
