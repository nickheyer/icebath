package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	opencodezenconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/opencodezen/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: opencodezenconfig.ZenName, Key: opencodezenconfig.ZenAPIKey, Variables: []string{opencodezenconfig.ZenAPIKeyEnv}})
	registry.Register(credentials.Keyed{Config: cfg, Provider: opencodezenconfig.GoName, Key: opencodezenconfig.GoAPIKey, Variables: []string{opencodezenconfig.GoAPIKeyEnv}})
}
