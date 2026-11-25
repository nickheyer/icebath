package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	azurefoundryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/azurefoundry/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: azurefoundryconfig.Name, Key: azurefoundryconfig.APIKey, Variables: []string{azurefoundryconfig.APIKeyEnv}})
}
