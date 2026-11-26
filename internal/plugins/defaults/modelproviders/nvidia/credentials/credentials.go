package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	nvidiaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nvidia/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: nvidiaconfig.Name, Variables: []string{nvidiaconfig.APIKeyEnv}})
}
