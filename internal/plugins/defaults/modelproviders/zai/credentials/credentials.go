package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	zaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/zai/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: zaiconfig.Name, Variables: []string{zaiconfig.APIKeyEnv, zaiconfig.ZAIAPIKeyEnv, zaiconfig.ZDotAIAPIKeyEnv}})
}
