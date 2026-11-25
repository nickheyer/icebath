package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	lmstudioconfig "github.com/nickheyer/icebath/internal/plugins/defaults/lmstudio/config"
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
		Name:               lmstudioconfig.Name,
		DisplayName:        "LM Studio",
		BaseURL:            p.config.GetString(lmstudioconfig.BaseURL),
		BaseURLEnv:         lmstudioconfig.BaseURLEnv,
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{lmstudioconfig.APIKeyEnv},
		Aliases:            []string{"lm-studio", "lm_studio"},
		DefaultModel:       p.config.GetString(lmstudioconfig.DefaultModel),
		Local:              true,
		VisionToolMessages: true,
		ModelsDevID:        lmstudioconfig.Name,
	}}
}
