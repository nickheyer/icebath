package credentials

import (
	"github.com/nickheyer/icebath/internal/credentials"
	minimaxconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/config"
)

func Register(registry *credentials.Registry) {
	registry.Register(credentials.Keyed{Provider: minimaxconfig.GlobalName, Variables: []string{minimaxconfig.GlobalAPIKeyEnv}})
	registry.Register(credentials.Keyed{Provider: minimaxconfig.CNName, Variables: []string{minimaxconfig.CNAPIKeyEnv}})
	registry.Register(oauth{})
}
