package actual

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/actual/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/actual/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/actual/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/actual/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
