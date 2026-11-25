package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	geminiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gemini/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(geminiconfig.BaseURL)
	return []provider.Profile{{
		Name:               geminiconfig.Name,
		DisplayName:        "Google AI Studio",
		BaseURL:            base,
		BaseURLEnv:         geminiconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{geminiconfig.GoogleAPIKeyEnv, geminiconfig.GeminiAPIKeyEnv},
		Aliases:            []string{"google", "google-gemini", "google-ai-studio"},
		DefaultModel:       p.config.GetString(geminiconfig.DefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        geminiconfig.Name,
		DefaultAuxModel:    "gemini-3.6-flash",
	}}
}
