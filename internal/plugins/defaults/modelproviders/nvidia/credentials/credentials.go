package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	nvidiaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nvidia/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: nvidiaconfig.Name, Key: nvidiaconfig.APIKey, Variables: []string{nvidiaconfig.APIKeyEnv}})
}
