package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	zaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/zai/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: zaiconfig.Name, Key: zaiconfig.APIKey, Variables: []string{zaiconfig.APIKeyEnv, zaiconfig.ZAIAPIKeyEnv, zaiconfig.ZDotAIAPIKeyEnv}})
}
