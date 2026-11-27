package xai

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xai/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
	app.Register(registries.Connectors)
	cli.Register(registries.SignIns)
}
