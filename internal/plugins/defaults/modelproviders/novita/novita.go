package novita

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/novita/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/novita/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/novita/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
