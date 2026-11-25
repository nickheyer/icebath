package azurefoundry

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
