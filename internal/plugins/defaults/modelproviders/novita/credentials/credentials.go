package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	novitaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/novita/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: novitaconfig.Name, Key: novitaconfig.APIKey, Variables: []string{novitaconfig.APIKeyEnv}})
}
