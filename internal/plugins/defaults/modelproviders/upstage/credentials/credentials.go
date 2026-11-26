package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	upstageconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/upstage/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: upstageconfig.Name, Variables: []string{upstageconfig.APIKeyEnv}})
}
