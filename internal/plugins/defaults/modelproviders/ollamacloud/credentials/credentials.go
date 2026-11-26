package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	ollamacloudconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/ollamacloud/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: ollamacloudconfig.Name, Variables: []string{ollamacloudconfig.APIKeyEnv}})
}
