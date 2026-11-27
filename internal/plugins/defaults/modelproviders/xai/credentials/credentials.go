package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	xaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: xaiconfig.XAIName, Variables: []string{xaiconfig.APIKeyEnv}})
	registry.Register(oauth{})
}
