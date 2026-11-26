package provider

import (
	kilocodeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kilocode/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := kilocodeconfig.BaseURL
	return []provider.Spec{{
		Name:               kilocodeconfig.Name,
		DisplayName:        kilocodeconfig.Title,
		BaseURL:            base,
		BaseURLEnv:         kilocodeconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{kilocodeconfig.APIKeyEnv},
		Aliases:            []string{"kilo-code", "kilo", "kilo-gateway"},
		DefaultModel:       kilocodeconfig.DefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        kilocodeconfig.Name,
		DefaultAuxModel:    "google/gemini-3.6-flash",
	}}
}
