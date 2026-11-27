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
		Name:                      geminiconfig.Name,
		ExtraBody:                 extraBody,
		Resolve:                   resolve,
		ReplaysToolCallSignatures: true,
		Native:                    provider.Unprefixed,
		DisplayName:               geminiconfig.Title,
		Rank:                      4,
		Description:               "Gemini models, with a Google AI Studio API key",
		BaseURL:                   base,
		BaseURLEnv:                geminiconfig.BaseURLEnv,
		ModelsURL:                 provider.ModelsURL(base),
		Mode:                      provider.ModeChatCompletions,
		Auth:                      provider.AuthAPIKey,
		AuthType:                  provider.AuthTypeAPIKey,
		KeyEnv:                    []string{geminiconfig.GoogleAPIKeyEnv, geminiconfig.GeminiAPIKeyEnv},
		Aliases:                   []string{"google", "google-gemini", "google-ai-studio"},
		DefaultModel:              geminiconfig.DefaultModel,
		ModelListing:              true,
		HealthCheck:               true,
		VisionToolMessages:        true,
		ModelsDevID:               "google",
		StaticModels:              []string{"gemini-3.8-flash", "gemini-4-argon", "gemini-3.7-flash", "gemini-3.1-pro-preview", "gemini-3-pro-preview", "gemini-3.6-flash", "gemini-3.1-flash-lite-preview"},
		ModelsDevPreferred:        true,
		Prices:                    provider.GeminiPrices,
		ListPrices:                "googleapis.com",
		Billing:                   []provider.Billing{{Label: "Google AI", URL: "https://aistudio.google.com/app/billing"}},
		DefaultAuxModel:           "gemini-3.6-flash",
	}}
}
