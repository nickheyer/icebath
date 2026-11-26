package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	nebiustokenfactoryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nebiustokenfactory/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: nebiustokenfactoryconfig.Name, Variables: []string{nebiustokenfactoryconfig.APIKeyEnv, nebiustokenfactoryconfig.TokenFactoryAPIKeyEnv}})
}
