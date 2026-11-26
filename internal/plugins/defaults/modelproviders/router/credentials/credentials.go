package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	routerconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/router/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: routerconfig.Name, Variables: []string{routerconfig.APIKeyEnv, routerconfig.RouterAPIKeyEnv}})
}
