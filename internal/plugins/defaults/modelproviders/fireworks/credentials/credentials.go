package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	fireworksconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/fireworks/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: fireworksconfig.Name, Variables: []string{fireworksconfig.APIKeyEnv}})
}
