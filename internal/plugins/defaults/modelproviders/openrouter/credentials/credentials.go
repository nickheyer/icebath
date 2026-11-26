package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	openrouterconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: openrouterconfig.Name, Variables: []string{openrouterconfig.APIKeyEnv}})
}
