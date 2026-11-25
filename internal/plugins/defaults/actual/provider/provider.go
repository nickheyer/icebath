package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	actualconfig "github.com/nickheyer/icebath/internal/plugins/defaults/actual/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(actualconfig.BaseURL)
	return []provider.Profile{{
		Name:               actualconfig.Name,
		DisplayName:        "Actual Computer",
		Description:        "Actual Computer - hosted inference via api.actual.inc, or local offline inference via model.base_url in config.yaml",
		SignupURL:          "https://actual.inc",
		BaseURL:            base,
		BaseURLEnv:         actualconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(actualconfig.NormalizeBaseURL(base)),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{actualconfig.APIKeyEnv},
		Aliases:            []string{"actual-computer", "actualcomputer", "aci"},
		DefaultModel:       p.config.GetString(actualconfig.DefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        actualconfig.Name,
	}}
}
