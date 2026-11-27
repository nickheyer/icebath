package provider

import (
	stepfunconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/stepfun/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:               stepfunconfig.Name,
		DisplayName:        stepfunconfig.Title,
		BaseURL:            stepfunconfig.BaseURL,
		BaseURLEnv:         stepfunconfig.BaseURLEnv,
		ModelsURL:          stepfunconfig.ModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{stepfunconfig.APIKeyEnv},
		Aliases:            []string{"step", "stepfun-coding-plan"},
		DefaultModel:       stepfunconfig.DefaultModel,
		DefaultAuxModel:    "step-3.5-flash",
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        stepfunconfig.Name,
		StaticModels:       []string{"step-3.7-flash", "step-3.5-flash", "step-3.5-flash-2603"},
		LiveFirst:          true,
		MetadataHostnames:  []string{"api.stepfun.com"},
	}}
}
