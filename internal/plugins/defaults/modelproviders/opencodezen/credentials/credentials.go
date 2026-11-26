package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	opencodezenconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/opencodezen/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: opencodezenconfig.ZenName, Variables: []string{opencodezenconfig.ZenAPIKeyEnv}})
	registry.Register(credentials.Keyed{Provider: opencodezenconfig.GoName, Variables: []string{opencodezenconfig.GoAPIKeyEnv}})
}
