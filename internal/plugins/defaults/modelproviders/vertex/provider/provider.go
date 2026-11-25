package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	vertexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/vertex/config"
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
		Name:               vertexconfig.Name,
		DisplayName:        "Google Vertex AI",
		BaseURL:            p.config.GetString(vertexconfig.BaseURL),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthGCPOAuth,
		AuthType:           provider.AuthTypeVertex,
		Aliases:            []string{"google-vertex", "vertex-ai", "gcp-vertex", "vertexai"},
		DefaultModel:       p.config.GetString(vertexconfig.DefaultModel),
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        vertexconfig.Name,
		DefaultAuxModel:    "google/gemini-3.6-flash",
	}}
}
