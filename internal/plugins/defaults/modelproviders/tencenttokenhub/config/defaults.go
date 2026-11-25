package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.SetDefault(TokenHubBaseURL, "https://tokenhub.tencentmaas.com/v1")
	cfg.SetDefault(TokenHubDefaultModel, "")
	cfg.MustBindEnv(TokenHubBaseURL, TokenHubBaseURLEnv)
	cfg.MustBindEnv(TokenHubAPIKey, TokenHubAPIKeyEnv)

	cfg.SetDefault(TokenPlanBaseURL, "https://api.lkeap.cloud.tencent.com/plan/anthropic")
	cfg.SetDefault(TokenPlanDefaultModel, "")
	cfg.MustBindEnv(TokenPlanBaseURL, TokenPlanBaseURLEnv)
	cfg.MustBindEnv(TokenPlanAPIKey, TokenPlanAPIKeyEnv)
}
