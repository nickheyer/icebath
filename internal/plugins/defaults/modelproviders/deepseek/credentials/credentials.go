package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	deepseekconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepseek/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: deepseekconfig.Name, Variables: []string{deepseekconfig.APIKeyEnv}})
}
