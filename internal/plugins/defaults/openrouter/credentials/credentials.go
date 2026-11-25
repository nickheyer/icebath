package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	openrouterconfig "github.com/nickheyer/icebath/internal/plugins/defaults/openrouter/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: openrouterconfig.Name, Key: openrouterconfig.APIKey, Variables: []string{openrouterconfig.APIKeyEnv}})
}
