package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	xiaomiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xiaomi/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: xiaomiconfig.Name, Key: xiaomiconfig.APIKey, Variables: []string{xiaomiconfig.APIKeyEnv}})
}
