package router

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/router/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/router/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/router/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/router/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
