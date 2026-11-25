package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	deepinfraconfig "github.com/nickheyer/icebath/internal/plugins/defaults/deepinfra/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: deepinfraconfig.Name, Key: deepinfraconfig.APIKey, Variables: []string{deepinfraconfig.APIKeyEnv}})
}
