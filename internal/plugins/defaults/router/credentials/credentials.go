package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	routerconfig "github.com/nickheyer/icebath/internal/plugins/defaults/router/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: routerconfig.Name, Key: routerconfig.APIKey, Variables: []string{routerconfig.APIKeyEnv, routerconfig.RouterAPIKeyEnv}})
}
