package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	azurefoundryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: azurefoundryconfig.Name, Variables: []string{azurefoundryconfig.APIKeyEnv}})
}
