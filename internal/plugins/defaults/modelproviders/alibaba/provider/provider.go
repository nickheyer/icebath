package provider

import (
	alibabaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibaba/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:               alibabaconfig.IntlName,
		DisplayName:        alibabaconfig.IntlTitle,
		BaseURL:            alibabaconfig.IntlBaseURL,
		BaseURLEnv:         alibabaconfig.DashScopeBaseURLEnv,
		ModelsURL:          alibabaconfig.IntlModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{alibabaconfig.DashScopeAPIKeyEnv},
		Aliases:            []string{"dashscope", "alibaba-cloud", "qwen-dashscope", "aliyun"},
		DefaultModel:       alibabaconfig.IntlDefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        alibabaconfig.IntlName,
	}, {
		Name:               alibabaconfig.CNName,
		DisplayName:        alibabaconfig.CNTitle,
		Description:        "DashScope, mainland China endpoint",
		BaseURL:            alibabaconfig.CNBaseURL,
		BaseURLEnv:         alibabaconfig.DashScopeCNBaseURLEnv,
		ModelsURL:          alibabaconfig.CNModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{alibabaconfig.DashScopeAPIKeyEnv},
		Aliases:            []string{"dashscope-cn", "alibaba-cloud-cn"},
		DefaultModel:       alibabaconfig.CNDefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        alibabaconfig.CNName,
	}, {
		Name:               alibabaconfig.TokenPlanName,
		DisplayName:        alibabaconfig.TokenPlanTitle,
		Description:        "Model Studio flat-rate token plan",
		SignupURL:          "https://help.aliyun.com/zh/model-studio/",
		BaseURL:            alibabaconfig.TokenPlanBaseURL,
		BaseURLEnv:         alibabaconfig.TokenPlanBaseURLEnv,
		ModelsURL:          alibabaconfig.TokenPlanModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{alibabaconfig.TokenPlanAPIKeyEnv},
		Aliases:            []string{"dashscope-token-plan"},
		DefaultModel:       alibabaconfig.TokenPlanDefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        alibabaconfig.TokenPlanName,
	}, {
		Name:               alibabaconfig.TokenPlanCNName,
		DisplayName:        alibabaconfig.TokenPlanCNTitle,
		Description:        "Model Studio flat-rate token plan, mainland China endpoint",
		SignupURL:          "https://help.aliyun.com/zh/model-studio/",
		BaseURL:            alibabaconfig.TokenPlanCNBaseURL,
		BaseURLEnv:         alibabaconfig.TokenPlanCNBaseURLEnv,
		ModelsURL:          alibabaconfig.TokenPlanCNModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{alibabaconfig.TokenPlanCNAPIKeyEnv, alibabaconfig.TokenPlanAPIKeyEnv},
		Aliases:            []string{"dashscope-token-plan-cn"},
		DefaultModel:       alibabaconfig.TokenPlanCNDefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        alibabaconfig.TokenPlanCNName,
	}}
}
