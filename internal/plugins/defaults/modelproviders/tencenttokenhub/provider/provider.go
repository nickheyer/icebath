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
		DisplayName:        tencenttokenhubconfig.TokenHubTitle,
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
		ModelsDevID:        tencenttokenhubconfig.TokenHubName,
	}, {
		Name:               tencenttokenhubconfig.TokenPlanName,
		DisplayName:        tencenttokenhubconfig.TokenPlanTitle,
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
		ModelsDevID:        tencenttokenhubconfig.TokenPlanName,
	}}
}
