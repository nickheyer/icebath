package provider

import (
	vertexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/vertex/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:               vertexconfig.Name,
		DisplayName:        vertexconfig.Title,
		BaseURL:            vertexconfig.BaseURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthGCPOAuth,
		AuthType:           provider.AuthTypeVertex,
		Aliases:            []string{"google-vertex", "vertex-ai", "gcp-vertex", "vertexai"},
		DefaultModel:       vertexconfig.DefaultModel,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        vertexconfig.Name,
		DefaultAuxModel:    "google/gemini-3.6-flash",
	}}
}
