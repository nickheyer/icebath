package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	kilocodeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/kilocode/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: kilocodeconfig.Name, Key: kilocodeconfig.APIKey, Variables: []string{kilocodeconfig.APIKeyEnv}})
}
