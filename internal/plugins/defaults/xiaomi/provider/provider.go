package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	xiaomiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/xiaomi/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	base := p.config.GetString(xiaomiconfig.BaseURL)
	return []provider.Profile{{
		Name:         xiaomiconfig.Name,
		DisplayName:  "Xiaomi MiMo",
		BaseURL:      base,
		BaseURLEnv:   xiaomiconfig.BaseURLEnv,
		ModelsURL:    provider.ModelsURL(base),
		Mode:         provider.ModeChatCompletions,
		Auth:         provider.AuthAPIKey,
		AuthType:     provider.AuthTypeAPIKey,
		KeyEnv:       []string{xiaomiconfig.APIKeyEnv},
		Aliases:      []string{"mimo", "xiaomi-mimo"},
		DefaultModel: p.config.GetString(xiaomiconfig.DefaultModel),
		ModelListing: true,
		Vision:       true,
		ModelsDevID:  xiaomiconfig.Name,
	}}
}
