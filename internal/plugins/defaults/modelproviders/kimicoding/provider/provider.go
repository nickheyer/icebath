package provider

import (
	kimicodingconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kimicoding/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	return []provider.Spec{{
		Name:               kimicodingconfig.GlobalName,
		Blind:              true,
		Shape:              shape,
		RequestHeaders:     headers,
		EchoesReasoning:    true,
		Route:              codingRoute(kimicodingconfig.GlobalBaseURL),
		Native:             provider.Unprefixed,
		DisplayName:        kimicodingconfig.GlobalTitle,
		Description:        "Kimi models from Moonshot, with an API key",
		BaseURL:            kimicodingconfig.GlobalBaseURL,
		BaseURLEnv:         kimicodingconfig.GlobalBaseURLEnv,
		ModelsURL:          kimicodingconfig.GlobalModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{kimicodingconfig.GlobalAPIKeyEnv, kimicodingconfig.GlobalCodingAPIKeyEnv},
		Aliases:            []string{"kimi", "moonshot", "kimi-for-coding"},
		DefaultModel:       kimicodingconfig.GlobalDefaultModel,
		DefaultAuxModel:    "kimi-k2-turbo-preview",
		Headers:            map[string]string{"HTTP-Referer": "https://github.com/nickheyer/icebath", "X-Title": "Icebath Agent", "User-Agent": provider.UserAgent("IcebathAgent"), "Accept-Encoding": "gzip"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		OmitTemperature:    true,
		DefaultMaxTokens:   32000,
		ModelsDevID:        "kimi-for-coding",
		StaticModels:       []string{"kimi-k3", "kimi-k2.7-code", "kimi-k2.6", "kimi-k2.5", "kimi-for-coding", "kimi-for-coding-highspeed", "kimi-k2-thinking", "kimi-k2-thinking-turbo", "kimi-k2-turbo-preview", "kimi-k2-0905-preview"},
		MetadataHostnames:  []string{"api.kimi.com"},
		ContextLength:      contextLength,
		Billing:            []provider.Billing{{Label: "Moonshot AI", URL: "https://platform.moonshot.ai/console/pay"}},
	}, {
		Name:               kimicodingconfig.CNName,
		Blind:              true,
		Shape:              shape,
		RequestHeaders:     headers,
		EchoesReasoning:    true,
		Route:              codingRoute(kimicodingconfig.CNBaseURL),
		Native:             provider.Unprefixed,
		DisplayName:        kimicodingconfig.CNTitle,
		Description:        "Kimi models from Moonshot, with an API key, served from mainland China",
		BaseURL:            kimicodingconfig.CNBaseURL,
		ModelsURL:          kimicodingconfig.CNModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{kimicodingconfig.CNAPIKeyEnv},
		Aliases:            []string{"kimi-cn", "moonshot-cn"},
		DefaultModel:       kimicodingconfig.CNDefaultModel,
		DefaultAuxModel:    "kimi-k2-turbo-preview",
		Headers:            map[string]string{"HTTP-Referer": "https://github.com/nickheyer/icebath", "X-Title": "Icebath Agent", "User-Agent": provider.UserAgent("IcebathAgent"), "Accept-Encoding": "gzip"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		OmitTemperature:    true,
		DefaultMaxTokens:   32000,
		ModelsDevID:        "kimi-for-coding",
		StaticModels:       []string{"kimi-k3", "kimi-k2.7-code", "kimi-k2.7-code-highspeed", "kimi-k2.6", "kimi-k2.5", "kimi-k2-thinking", "kimi-k2-turbo-preview", "kimi-k2-0905-preview"},
	}}
}
