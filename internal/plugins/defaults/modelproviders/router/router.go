package router

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/router/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/router/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/router/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
