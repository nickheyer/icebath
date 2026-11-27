package minimax

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
	app.Register(registries.Connectors)
	cli.Register(registries.SignIns)
}
