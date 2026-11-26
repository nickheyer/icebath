package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	xiaomiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xiaomi/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: xiaomiconfig.Name, Variables: []string{xiaomiconfig.APIKeyEnv}})
}
