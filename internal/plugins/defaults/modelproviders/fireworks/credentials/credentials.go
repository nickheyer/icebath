package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	fireworksconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/fireworks/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: fireworksconfig.Name, Key: fireworksconfig.APIKey, Variables: []string{fireworksconfig.APIKeyEnv}})
}
