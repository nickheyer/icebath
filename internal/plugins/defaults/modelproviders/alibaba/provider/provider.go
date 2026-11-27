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
		Caching:            caching,
		CacheTTL:           cacheTTL,
		DottedModels:       true,
		Shape:              shape,
		Native:             provider.Unprefixed,
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
		StaticModels:       []string{"qwen3.8-max", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus", "qwen3.6-flash", "kimi-k2.5", "qwen3.5-plus", "qwen3-coder-plus", "qwen3-coder-next", "glm-5.2", "glm-5", "glm-4.7", "deepseek-v4-pro", "deepseek-v4-flash-0731", "MiniMax-M2.5"},
		MetadataHostnames:  []string{"dashscope.aliyuncs.com", "dashscope-intl.aliyuncs.com"},
	}, {
		Name:               alibabaconfig.CNName,
		Shape:              shape,
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
		StaticModels:       []string{"qwen3.8-max", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus", "qwen3.6-flash", "kimi-k2.5", "qwen3.5-plus", "qwen3-coder-plus", "qwen3-coder-next", "glm-5.2", "glm-5", "glm-4.7", "deepseek-v4-pro", "deepseek-v4-flash-0731", "MiniMax-M2.5"},
	}, {
		Name:               alibabaconfig.TokenPlanName,
		Shape:              shape,
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
		StaticModels:       []string{"qwen3.8-max-0902", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus", "qwen3.6-flash", "deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v3.2", "kimi-k2.7-code", "kimi-k2.6", "kimi-k2.5", "glm-5.2", "glm-5.1", "glm-5"},
	}, {
		Name:               alibabaconfig.TokenPlanCNName,
		Shape:              shape,
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
		StaticModels:       []string{"qwen3.8-max-0902", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus", "qwen3.6-flash", "deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v3.2", "kimi-k2.7-code", "kimi-k2.6", "kimi-k2.5", "glm-5.2", "glm-5.1", "glm-5"},
	}}
}
