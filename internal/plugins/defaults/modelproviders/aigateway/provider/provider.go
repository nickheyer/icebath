package provider

import (
	aigatewayconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/aigateway/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := aigatewayconfig.BaseURL
	return []provider.Spec{{
		Name:               aigatewayconfig.Name,
		Shape:              shape,
		Carriers:           []string{"reasoning_content", "reasoning", "reasoning_details"},
		Native:             provider.Vendored,
		DisplayName:        aigatewayconfig.Title,
		Description:        "Many companies' models behind one Vercel API key",
		BaseURL:            base,
		BaseURLEnv:         aigatewayconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{aigatewayconfig.APIKeyEnv},
		Aliases:            []string{"vercel", "vercel-ai-gateway", "ai_gateway", "aigateway"},
		DefaultModel:       aigatewayconfig.DefaultModel,
		Headers:            map[string]string{"HTTP-Referer": "https://github.com/nickheyer/icebath", "X-Title": "Icebath Agent"},
		Aggregator:         true,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        "vercel",
		DefaultAuxModel:    "google/gemini-3-flash",
		StaticModels: []string{
			"moonshotai/kimi-k2.6", "alibaba/qwen3.6-plus", "zai/glm-5.1", "minimax/minimax-m2.7", "anthropic/claude-sonnet-4.6",
			"anthropic/claude-opus-4.7", "anthropic/claude-opus-4.6", "anthropic/claude-haiku-4.5",
			"openai/gpt-5.4", "openai/gpt-5.4-mini", "openai/gpt-5.3-codex", "google/gemini-3.1-pro-preview",
			"google/gemini-3-flash", "google/gemini-3.1-flash-lite-preview", "xai/grok-4.20-reasoning",
		},
	}}
}
