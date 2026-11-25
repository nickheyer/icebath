package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	kimicodingconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kimicoding/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: kimicodingconfig.GlobalName, Key: kimicodingconfig.GlobalAPIKey, Variables: []string{kimicodingconfig.GlobalAPIKeyEnv, kimicodingconfig.GlobalCodingAPIKeyEnv}})
	registry.Register(credentials.Keyed{Config: cfg, Provider: kimicodingconfig.CNName, Key: kimicodingconfig.CNAPIKey, Variables: []string{kimicodingconfig.CNAPIKeyEnv}})
}
