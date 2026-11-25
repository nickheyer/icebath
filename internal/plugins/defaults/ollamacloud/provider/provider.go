package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	ollamacloudconfig "github.com/nickheyer/icebath/internal/plugins/defaults/ollamacloud/config"
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
		Name:               ollamacloudconfig.Name,
		DisplayName:        "Ollama Cloud",
		BaseURL:            p.config.GetString(ollamacloudconfig.BaseURL),
		BaseURLEnv:         ollamacloudconfig.BaseURLEnv,
		ModelsURL:          ollamacloudconfig.ModelsURL,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{ollamacloudconfig.APIKeyEnv},
		Aliases:            []string{"ollama_cloud"},
		DefaultModel:       p.config.GetString(ollamacloudconfig.DefaultModel),
		DefaultAuxModel:    "nemotron-3-nano:30b",
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        ollamacloudconfig.Name,
	}}
}
