package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	lmstudioconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/lmstudio/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: lmstudioconfig.Name, Key: lmstudioconfig.APIKey, Variables: []string{lmstudioconfig.APIKeyEnv}})
}
