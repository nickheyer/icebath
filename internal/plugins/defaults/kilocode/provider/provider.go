package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	kilocodeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/kilocode/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(kilocodeconfig.BaseURL)
	return []provider.Profile{{
		Name:               kilocodeconfig.Name,
		DisplayName:        "Kilo Code",
		BaseURL:            base,
		BaseURLEnv:         kilocodeconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{kilocodeconfig.APIKeyEnv},
		Aliases:            []string{"kilo-code", "kilo", "kilo-gateway"},
		DefaultModel:       p.config.GetString(kilocodeconfig.DefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        kilocodeconfig.Name,
		DefaultAuxModel:    "google/gemini-3.6-flash",
	}}
}
