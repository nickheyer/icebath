package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	arceeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/arcee/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: arceeconfig.Name, Variables: []string{arceeconfig.APIKeyEnv}})
}
