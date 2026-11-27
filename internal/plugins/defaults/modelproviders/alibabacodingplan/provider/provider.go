package provider

import (
	alibabacodingplanconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibabacodingplan/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:               alibabacodingplanconfig.IntlName,
		Shape:              shape,
		DisplayName:        alibabacodingplanconfig.IntlTitle,
		Description:        "Dedicated coding tier",
		SignupURL:          "https://help.aliyun.com/zh/model-studio/",
		BaseURL:            alibabacodingplanconfig.IntlBaseURL,
		BaseURLEnv:         alibabacodingplanconfig.CodingPlanBaseURLEnv,
		ModelsURL:          alibabacodingplanconfig.IntlModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{alibabacodingplanconfig.CodingPlanAPIKeyEnv, alibabacodingplanconfig.DashScopeAPIKeyEnv},
		Aliases:            []string{"alibaba_coding", "alibaba-coding", "dashscope-coding", "alibaba_coding_plan"},
		DefaultModel:       alibabacodingplanconfig.IntlDefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		StaticModels:       []string{"qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus", "qwen3-max-2026-01-23", "qwen3-coder-plus", "qwen3-coder-next", "kimi-k2.5", "glm-5", "glm-4.7", "MiniMax-M2.5"},
	}, {
		Name:               alibabacodingplanconfig.CNName,
		Shape:              shape,
		DisplayName:        alibabacodingplanconfig.CNTitle,
		Description:        "Dedicated coding tier, mainland China endpoint",
		SignupURL:          "https://help.aliyun.com/zh/model-studio/",
		BaseURL:            alibabacodingplanconfig.CNBaseURL,
		BaseURLEnv:         alibabacodingplanconfig.CodingPlanCNBaseURLEnv,
		ModelsURL:          alibabacodingplanconfig.CNModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{alibabacodingplanconfig.CodingPlanCNAPIKeyEnv, alibabacodingplanconfig.CodingPlanAPIKeyEnv, alibabacodingplanconfig.DashScopeAPIKeyEnv},
		Aliases:            []string{"alibaba-coding-cn", "dashscope-coding-cn"},
		DefaultModel:       alibabacodingplanconfig.CNDefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		StaticModels:       []string{"qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus", "qwen3-max-2026-01-23", "qwen3-coder-plus", "qwen3-coder-next", "kimi-k2.5", "glm-5", "glm-4.7", "MiniMax-M2.5"},
	}}
}
