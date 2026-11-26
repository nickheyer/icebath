package provider

import (
	geminiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gemini/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := geminiconfig.BaseURL
	return []provider.Spec{{
		Name:               geminiconfig.Name,
		DisplayName:        geminiconfig.Title,
		BaseURL:            base,
		BaseURLEnv:         geminiconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{geminiconfig.GoogleAPIKeyEnv, geminiconfig.GeminiAPIKeyEnv},
		Aliases:            []string{"google", "google-gemini", "google-ai-studio"},
		DefaultModel:       geminiconfig.DefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        geminiconfig.Name,
		DefaultAuxModel:    "gemini-3.6-flash",
	}}
}
