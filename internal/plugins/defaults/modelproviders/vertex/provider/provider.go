package provider

import (
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	vertexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/vertex/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:                      vertexconfig.Name,
		DottedModels:              true,
		PerModelMode:              mode,
		ExtraBody:                 extraBody,
		ReplaysToolCallSignatures: true,
		DisplayName:               vertexconfig.Title,
		BaseURL:                   vertexconfig.BaseURL,
		Mode:                      provider.ModeChatCompletions,
		Auth:                      provider.AuthGCPOAuth,
		AuthType:                  provider.AuthTypeVertex,
		Aliases:                   []string{"google-vertex", "vertex-ai", "gcp-vertex", "vertexai"},
		DefaultModel:              vertexconfig.DefaultModel,
		HealthCheck:               true,
		VisionToolMessages:        true,
		StaticModels:              []string{"google/gemini-3.8-flash", "google/gemini-4-argon", "google/gemini-3.7-flash", "google/gemini-3.1-pro-preview", "google/gemini-3-pro-preview", "google/gemini-3.6-flash", "google/gemini-3.5-flash", "google/gemini-3.5-flash-lite", "google/gemini-3-flash-preview", "google/gemini-3.1-flash-lite-preview", "google/gemini-3.1-flash-lite"},
		Prices:                    provider.GeminiPrices,
		ListPrices:                "googleapis.com",
		DefaultAuxModel:           "google/gemini-3.6-flash",
		Settings: []config.Setting{
			{Key: vertexconfig.Project, Title: "Vertex project", About: "Google Cloud project of the requests; the project of the credentials when empty",
				Default: "", Env: []string{vertexconfig.ProjectEnv}},
			{Key: vertexconfig.Region, Title: "Vertex region", About: "Region of the requests, such as us-central1, or global", Default: vertexconfig.DefaultRegion,
				Env: []string{vertexconfig.RegionEnv}},
		},
		Account: []config.Setting{{Key: vertexconfig.CredentialsPath, Title: "Service account key", About: "Path to a Google service account JSON key",
			Default: ""}},
	}}
}

// Claude on Vertex is served only by Anthropic's publisher endpoint, over the Anthropic Messages API; every other model by the OpenAI
// compatible one
func mode(model string) provider.APIMode {
	if strings.Contains(strings.ToLower(model), "claude") {
		return provider.ModeAnthropicMessages
	}
	return provider.ModeChatCompletions
}
