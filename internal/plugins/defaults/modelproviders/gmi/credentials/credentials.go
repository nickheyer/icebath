package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	gmiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gmi/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: gmiconfig.Name, Variables: []string{gmiconfig.APIKeyEnv}})
}
