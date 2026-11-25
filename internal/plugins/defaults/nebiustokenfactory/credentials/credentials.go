package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	nebiustokenfactoryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/nebiustokenfactory/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: nebiustokenfactoryconfig.Name, Key: nebiustokenfactoryconfig.APIKey, Variables: []string{nebiustokenfactoryconfig.APIKeyEnv, nebiustokenfactoryconfig.TokenFactoryAPIKeyEnv}})
}
