package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	alibabaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/alibaba/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	return []provider.Profile{{
		Name:               alibabaconfig.IntlName,
		DisplayName:        "Qwen Cloud",
		BaseURL:            p.config.GetString(alibabaconfig.IntlBaseURL),
		BaseURLEnv:         alibabaconfig.DashScopeBaseURLEnv,
		ModelsURL:          alibabaconfig.IntlModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{alibabaconfig.DashScopeAPIKeyEnv},
		Aliases:            []string{"dashscope", "alibaba-cloud", "qwen-dashscope", "aliyun"},
		DefaultModel:       p.config.GetString(alibabaconfig.IntlDefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        alibabaconfig.IntlName,
	}, {
		Name:               alibabaconfig.CNName,
		DisplayName:        "Alibaba Cloud DashScope (China)",
		Description:        "Alibaba Cloud DashScope, mainland-China endpoint",
		BaseURL:            p.config.GetString(alibabaconfig.CNBaseURL),
		BaseURLEnv:         alibabaconfig.DashScopeCNBaseURLEnv,
		ModelsURL:          alibabaconfig.CNModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{alibabaconfig.DashScopeAPIKeyEnv},
		Aliases:            []string{"dashscope-cn", "alibaba-cloud-cn"},
		DefaultModel:       p.config.GetString(alibabaconfig.CNDefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        alibabaconfig.CNName,
	}, {
		Name:               alibabaconfig.TokenPlanName,
		DisplayName:        "Alibaba Cloud (Token Plan)",
		Description:        "Alibaba Cloud Model Studio Token Plan (flat-token tier)",
		SignupURL:          "https://help.aliyun.com/zh/model-studio/",
		BaseURL:            p.config.GetString(alibabaconfig.TokenPlanBaseURL),
		BaseURLEnv:         alibabaconfig.TokenPlanBaseURLEnv,
		ModelsURL:          alibabaconfig.TokenPlanModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{alibabaconfig.TokenPlanAPIKeyEnv},
		Aliases:            []string{"dashscope-token-plan"},
		DefaultModel:       p.config.GetString(alibabaconfig.TokenPlanDefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        alibabaconfig.TokenPlanName,
	}, {
		Name:               alibabaconfig.TokenPlanCNName,
		DisplayName:        "Alibaba Cloud (Token Plan, China)",
		Description:        "Alibaba Cloud Model Studio Token Plan, mainland-China endpoint",
		SignupURL:          "https://help.aliyun.com/zh/model-studio/",
		BaseURL:            p.config.GetString(alibabaconfig.TokenPlanCNBaseURL),
		BaseURLEnv:         alibabaconfig.TokenPlanCNBaseURLEnv,
		ModelsURL:          alibabaconfig.TokenPlanCNModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{alibabaconfig.TokenPlanCNAPIKeyEnv, alibabaconfig.TokenPlanAPIKeyEnv},
		Aliases:            []string{"dashscope-token-plan-cn"},
		DefaultModel:       p.config.GetString(alibabaconfig.TokenPlanCNDefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        alibabaconfig.TokenPlanCNName,
	}}
}
