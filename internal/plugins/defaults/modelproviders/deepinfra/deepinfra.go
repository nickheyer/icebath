package deepinfra

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepinfra/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepinfra/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepinfra/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
