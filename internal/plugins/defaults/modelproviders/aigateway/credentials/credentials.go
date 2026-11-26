package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	aigatewayconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/aigateway/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: aigatewayconfig.Name, Variables: []string{aigatewayconfig.APIKeyEnv}})
}
