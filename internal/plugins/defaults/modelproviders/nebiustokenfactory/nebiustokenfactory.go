package nebiustokenfactory

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nebiustokenfactory/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nebiustokenfactory/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nebiustokenfactory/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nebiustokenfactory/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
