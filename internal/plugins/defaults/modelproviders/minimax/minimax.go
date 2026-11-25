package minimax

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/minimax/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
