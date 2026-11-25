package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	tencenttokenhubconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tencenttokenhub/config"
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
		Name:               tencenttokenhubconfig.TokenHubName,
		DisplayName:        "Tencent TokenHub",
		BaseURL:            p.config.GetString(tencenttokenhubconfig.TokenHubBaseURL),
		BaseURLEnv:         tencenttokenhubconfig.TokenHubBaseURLEnv,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{tencenttokenhubconfig.TokenHubAPIKeyEnv},
		Aliases:            []string{"tencent", "tokenhub", "tencent-cloud", "tencentmaas"},
		DefaultModel:       p.config.GetString(tencenttokenhubconfig.TokenHubDefaultModel),
		ModelListing:       true,
		VisionToolMessages: true,
		ModelsDevID:        tencenttokenhubconfig.TokenHubName,
	}, {
		Name:               tencenttokenhubconfig.TokenPlanName,
		DisplayName:        "Tencent TokenPlan",
		BaseURL:            p.config.GetString(tencenttokenhubconfig.TokenPlanBaseURL),
		BaseURLEnv:         tencenttokenhubconfig.TokenPlanBaseURLEnv,
		ModelsURL:          tencenttokenhubconfig.TokenPlanModelsURL,
		Mode:               provider.ModeAnthropicMessages,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{tencenttokenhubconfig.TokenPlanAPIKeyEnv},
		Aliases:            []string{"tokenplan", "tencent-lkeap"},
		DefaultModel:       p.config.GetString(tencenttokenhubconfig.TokenPlanDefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        tencenttokenhubconfig.TokenPlanName,
	}}
}
