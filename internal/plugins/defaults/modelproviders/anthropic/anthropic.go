package anthropic

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/anthropic/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
	app.Register(registries.App, registries.Config)
}
