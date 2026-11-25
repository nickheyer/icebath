package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	upstageconfig "github.com/nickheyer/icebath/internal/plugins/defaults/upstage/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: upstageconfig.Name, Key: upstageconfig.APIKey, Variables: []string{upstageconfig.APIKeyEnv}})
}
