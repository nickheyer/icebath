package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	novitaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/novita/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: novitaconfig.Name, Variables: []string{novitaconfig.APIKeyEnv}})
}
