package azurefoundry

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
