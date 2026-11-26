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
		DisplayName:        kimicodingconfig.GlobalTitle,
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
		Headers:            map[string]string{"HTTP-Referer": "https://github.com/nickheyer/icebath", "X-Title": "Icebath Agent", "User-Agent": "IcebathAgent/dev", "Accept-Encoding": "gzip"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		OmitTemperature:    true,
		DefaultMaxTokens:   32000,
		ModelsDevID:        "kimi-for-coding",
	}, {
		Name:               kimicodingconfig.CNName,
		DisplayName:        kimicodingconfig.CNTitle,
		BaseURL:            kimicodingconfig.CNBaseURL,
		ModelsURL:          kimicodingconfig.CNModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{kimicodingconfig.CNAPIKeyEnv},
		Aliases:            []string{"kimi-cn", "moonshot-cn"},
		DefaultModel:       kimicodingconfig.CNDefaultModel,
		DefaultAuxModel:    "kimi-k2-turbo-preview",
		Headers:            map[string]string{"HTTP-Referer": "https://github.com/nickheyer/icebath", "X-Title": "Icebath Agent", "User-Agent": "IcebathAgent/dev", "Accept-Encoding": "gzip"},
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		OmitTemperature:    true,
		DefaultMaxTokens:   32000,
		ModelsDevID:        "kimi-for-coding",
	}}
}
