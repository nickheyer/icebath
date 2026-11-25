package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	xaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/xai/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: xaiconfig.XAIName, Key: xaiconfig.XAIAPIKey, Variables: []string{xaiconfig.APIKeyEnv}})
}
