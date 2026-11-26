package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	metaaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/metaai/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: metaaiconfig.Name, Variables: []string{metaaiconfig.APIKeyEnv, metaaiconfig.MetaAPIKeyEnv, metaaiconfig.MetaModelAPIKeyEnv}})
}
