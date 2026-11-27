package provider

import (
	arceeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/arcee/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:               arceeconfig.Name,
		Native:             provider.Unprefixed,
		DisplayName:        arceeconfig.Title,
		BaseURL:            arceeconfig.BaseURL,
		BaseURLEnv:         arceeconfig.BaseURLEnv,
		ModelsURL:          arceeconfig.ModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{arceeconfig.APIKeyEnv},
		Aliases:            []string{"arcee-ai", "arceeai"},
		DefaultModel:       arceeconfig.DefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		StaticModels:       []string{"trinity-large-thinking", "trinity-large-preview", "trinity-mini"},
	}}
}
