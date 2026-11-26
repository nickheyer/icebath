package anthropic

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
	app.Register(registries.Connectors)
}
