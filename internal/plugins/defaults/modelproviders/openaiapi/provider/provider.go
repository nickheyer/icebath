package provider

import (
	openaiapiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaiapi/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := openaiapiconfig.BaseURL
	return []provider.Spec{{
		Name:               openaiapiconfig.Name,
		ReservedTools:      []string{"tool_search"},
		DisplayName:        openaiapiconfig.Title,
		BaseURL:            base,
		BaseURLEnv:         openaiapiconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeCodexResponses,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{openaiapiconfig.APIKeyEnv},
		DefaultModel:       openaiapiconfig.DefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        "openai",
		ModelMetadata:      map[string]map[string]any{"gpt-6-astra": {"limit": map[string]any{"context": float64(1_050_000), "output": float64(128_000)}, "modalities": map[string]any{"input": []any{"text", "image"}, "output": []any{"text"}}, "tool_call": true, "reasoning": true, "family": "gpt-6"}},
		Prices:             prices,
		TierPrices:         map[string]map[string]provider.Price{"ultrafast": ultrafast},
		ListPrices:         "openai.com",
		Billing:            []provider.Billing{{Label: "OpenAI", URL: "https://platform.openai.com/settings/organization/billing"}},
		StaticModels: []string{
			"gpt-6.1-sol", "gpt-6.1-sol-pro", "gpt-6-sol", "gpt-6-sol-pro", "gpt-6-luna", "gpt-6-luna-pro",
			"gpt-5.6-sol", "gpt-5.6-sol-pro", "gpt-5.6-terra", "gpt-5.6-terra-pro", "gpt-5.6-luna",
			"gpt-5.6-luna-pro", "gpt-5.5", "gpt-5.5-pro", "gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano",
			"gpt-5-mini", "gpt-5.3-codex", "gpt-4.1", "gpt-4o", "gpt-4o-mini",
		},
	}}
}
