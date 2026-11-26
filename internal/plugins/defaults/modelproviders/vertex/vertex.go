package vertex

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/vertex/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
}
