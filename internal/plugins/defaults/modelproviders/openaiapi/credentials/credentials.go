package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	openaiapiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaiapi/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: openaiapiconfig.Name, Variables: []string{openaiapiconfig.APIKeyEnv}})
}
