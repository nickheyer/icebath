package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	kimicodingconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kimicoding/config"
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
		Name:               kimicodingconfig.GlobalName,
		DisplayName:        "Kimi / Moonshot",
		BaseURL:            p.config.GetString(kimicodingconfig.GlobalBaseURL),
		BaseURLEnv:         kimicodingconfig.GlobalBaseURLEnv,
		ModelsURL:          kimicodingconfig.GlobalModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{kimicodingconfig.GlobalAPIKeyEnv, kimicodingconfig.GlobalCodingAPIKeyEnv},
		Aliases:            []string{"kimi", "moonshot", "kimi-for-coding"},
		DefaultModel:       p.config.GetString(kimicodingconfig.GlobalDefaultModel),
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
		DisplayName:        "Kimi / Moonshot (China)",
		BaseURL:            p.config.GetString(kimicodingconfig.CNBaseURL),
		ModelsURL:          kimicodingconfig.CNModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{kimicodingconfig.CNAPIKeyEnv},
		Aliases:            []string{"kimi-cn", "moonshot-cn"},
		DefaultModel:       p.config.GetString(kimicodingconfig.CNDefaultModel),
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
