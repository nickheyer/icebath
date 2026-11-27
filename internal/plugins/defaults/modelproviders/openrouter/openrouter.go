package openrouter

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
	app.Register(registries.Connectors)
	cli.Register(registries.SignIns)
}
