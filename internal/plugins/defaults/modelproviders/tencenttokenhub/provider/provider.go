package provider

import (
	tencenttokenhubconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/tencenttokenhub/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:               tencenttokenhubconfig.TokenHubName,
		DefaultReasoning:   defaultReasoning,
		Shape:              shape,
		Carriers:           []string{"reasoning_content", "reasoning_details"},
		DefaultAuxModel:    "hy4-preview",
		Hostnames:          []string{"tokenhub.tencentmaas.com", "tokenhub-intl.tencentcloudmaas.com"},
		DisplayName:        tencenttokenhubconfig.TokenHubTitle,
		Description:        "Models hosted by Tencent Cloud, with a TokenHub API key",
		BaseURL:            tencenttokenhubconfig.TokenHubBaseURL,
		BaseURLEnv:         tencenttokenhubconfig.TokenHubBaseURLEnv,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{tencenttokenhubconfig.TokenHubAPIKeyEnv},
		Aliases:            []string{"tencent", "tokenhub", "tencent-cloud", "tencentmaas"},
		DefaultModel:       tencenttokenhubconfig.TokenHubDefaultModel,
		ModelListing:       true,
		VisionToolMessages: true,
		StaticModels:       []string{"hy4-preview", "hy3", "hy3-preview"},
	}, {
		Name:               tencenttokenhubconfig.TokenPlanName,
		DefaultAuxModel:    "hy4-preview",
		DisplayName:        tencenttokenhubconfig.TokenPlanTitle,
		Description:        "Models on a Tencent TokenPlan, with your plan's API key",
		BaseURL:            tencenttokenhubconfig.TokenPlanBaseURL,
		BaseURLEnv:         tencenttokenhubconfig.TokenPlanBaseURLEnv,
		ModelsURL:          tencenttokenhubconfig.TokenPlanModelsURL,
		Mode:               provider.ModeAnthropicMessages,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{tencenttokenhubconfig.TokenPlanAPIKeyEnv},
		Aliases:            []string{"tokenplan", "tencent-lkeap"},
		DefaultModel:       tencenttokenhubconfig.TokenPlanDefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		StaticModels:       []string{"hy4-preview", "hy3", "hy3-preview"},
	}}
}
