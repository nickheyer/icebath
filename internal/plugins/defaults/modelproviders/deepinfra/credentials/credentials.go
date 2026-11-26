package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	deepinfraconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepinfra/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: deepinfraconfig.Name, Variables: []string{deepinfraconfig.APIKeyEnv}})
}
