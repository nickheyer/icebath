package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	minimaxconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: minimaxconfig.GlobalName, Key: minimaxconfig.GlobalAPIKey, Variables: []string{minimaxconfig.GlobalAPIKeyEnv}})
	registry.Register(credentials.Keyed{Config: cfg, Provider: minimaxconfig.CNName, Key: minimaxconfig.CNAPIKey, Variables: []string{minimaxconfig.CNAPIKeyEnv}})
}
