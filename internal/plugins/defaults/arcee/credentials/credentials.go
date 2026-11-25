package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	arceeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/arcee/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: arceeconfig.Name, Key: arceeconfig.APIKey, Variables: []string{arceeconfig.APIKeyEnv}})
}
