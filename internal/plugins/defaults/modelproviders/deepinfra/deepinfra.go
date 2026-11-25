package deepinfra

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepinfra/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepinfra/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepinfra/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepinfra/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
