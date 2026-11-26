package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	kilocodeconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kilocode/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: kilocodeconfig.Name, Variables: []string{kilocodeconfig.APIKeyEnv}})
}
