package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	alibabacodingplanconfig "github.com/nickheyer/icebath/internal/plugins/defaults/alibabacodingplan/config"
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
		Name:               alibabacodingplanconfig.IntlName,
		DisplayName:        "Alibaba Cloud (Coding Plan)",
		Description:        "Alibaba Cloud Coding Plan (Dedicated coding tier)",
		SignupURL:          "https://help.aliyun.com/zh/model-studio/",
		BaseURL:            p.config.GetString(alibabacodingplanconfig.IntlBaseURL),
		BaseURLEnv:         alibabacodingplanconfig.CodingPlanBaseURLEnv,
		ModelsURL:          alibabacodingplanconfig.IntlModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{alibabacodingplanconfig.CodingPlanAPIKeyEnv, alibabacodingplanconfig.DashScopeAPIKeyEnv},
		Aliases:            []string{"alibaba_coding", "alibaba-coding", "dashscope-coding", "alibaba_coding_plan"},
		DefaultModel:       p.config.GetString(alibabacodingplanconfig.IntlDefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        alibabacodingplanconfig.IntlName,
	}, {
		Name:               alibabacodingplanconfig.CNName,
		DisplayName:        "Alibaba Cloud (Coding Plan, China)",
		Description:        "Alibaba Cloud Coding Plan, mainland-China endpoint",
		SignupURL:          "https://help.aliyun.com/zh/model-studio/",
		BaseURL:            p.config.GetString(alibabacodingplanconfig.CNBaseURL),
		BaseURLEnv:         alibabacodingplanconfig.CodingPlanCNBaseURLEnv,
		ModelsURL:          alibabacodingplanconfig.CNModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{alibabacodingplanconfig.CodingPlanCNAPIKeyEnv, alibabacodingplanconfig.CodingPlanAPIKeyEnv, alibabacodingplanconfig.DashScopeAPIKeyEnv},
		Aliases:            []string{"alibaba-coding-cn", "dashscope-coding-cn"},
		DefaultModel:       p.config.GetString(alibabacodingplanconfig.CNDefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        alibabacodingplanconfig.CNName,
	}}
}
