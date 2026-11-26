package fireworks

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/fireworks/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/fireworks/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/fireworks/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
