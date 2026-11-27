package azurefoundry

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
	app.Register(registries.Connectors)
	cli.Register(registries.SignIns)
}
