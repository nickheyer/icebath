package nebiustokenfactory

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nebiustokenfactory/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nebiustokenfactory/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nebiustokenfactory/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
