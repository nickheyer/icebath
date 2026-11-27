package provider

import (
	ollamacloudconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/ollamacloud/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:               ollamacloudconfig.Name,
		Shape:              shape,
		Carriers:           []string{"reasoning_content", "reasoning"},
		Native:             provider.Unprefixed,
		DisplayName:        ollamacloudconfig.Title,
		BaseURL:            ollamacloudconfig.BaseURL,
		BaseURLEnv:         ollamacloudconfig.BaseURLEnv,
		ModelsURL:          ollamacloudconfig.ModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{ollamacloudconfig.APIKeyEnv},
		Aliases:            []string{"ollama_cloud"},
		DefaultModel:       ollamacloudconfig.DefaultModel,
		DefaultAuxModel:    "nemotron-3-nano:30b",
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        ollamacloudconfig.Name,
		MetadataHostnames:  []string{"ollama.com"},
	}}
}
