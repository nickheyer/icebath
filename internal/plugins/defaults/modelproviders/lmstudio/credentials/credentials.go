package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	lmstudioconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/lmstudio/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: lmstudioconfig.Name, Variables: []string{lmstudioconfig.APIKeyEnv}})
}
