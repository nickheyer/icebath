package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	kimicodingconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kimicoding/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: kimicodingconfig.GlobalName, Variables: []string{kimicodingconfig.GlobalAPIKeyEnv, kimicodingconfig.GlobalCodingAPIKeyEnv}})
	registry.Register(credentials.Keyed{Provider: kimicodingconfig.CNName, Variables: []string{kimicodingconfig.CNAPIKeyEnv}})
}
