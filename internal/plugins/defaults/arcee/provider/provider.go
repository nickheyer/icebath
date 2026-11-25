package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	arceeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/arcee/config"
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
		Name:               arceeconfig.Name,
		DisplayName:        "Arcee AI",
		BaseURL:            p.config.GetString(arceeconfig.BaseURL),
		BaseURLEnv:         arceeconfig.BaseURLEnv,
		ModelsURL:          arceeconfig.ModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{arceeconfig.APIKeyEnv},
		Aliases:            []string{"arcee-ai", "arceeai"},
		DefaultModel:       p.config.GetString(arceeconfig.DefaultModel),
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        arceeconfig.Name,
	}}
}
