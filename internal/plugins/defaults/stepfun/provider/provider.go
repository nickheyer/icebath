package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	stepfunconfig "github.com/nickheyer/icebath/internal/plugins/defaults/stepfun/config"
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
		Name:               stepfunconfig.Name,
		DisplayName:        "StepFun Step Plan",
		BaseURL:            p.config.GetString(stepfunconfig.BaseURL),
		BaseURLEnv:         stepfunconfig.BaseURLEnv,
		ModelsURL:          stepfunconfig.ModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{stepfunconfig.APIKeyEnv},
		Aliases:            []string{"step", "stepfun-coding-plan"},
		DefaultModel:       p.config.GetString(stepfunconfig.DefaultModel),
		DefaultAuxModel:    "step-3.5-flash",
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        stepfunconfig.Name,
	}}
}
