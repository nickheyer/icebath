package credentials

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	metaaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/metaai/config"
)

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(credentials.Keyed{Config: cfg, Provider: metaaiconfig.Name, Key: metaaiconfig.APIKey, Variables: []string{metaaiconfig.APIKeyEnv, metaaiconfig.MetaAPIKeyEnv, metaaiconfig.MetaModelAPIKeyEnv}})
}
