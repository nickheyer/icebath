package openaicodex

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
	app.Register(registries.Connectors)
	cli.Register(registries.SignIns)
}
