package commandcode

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/commandcode/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/commandcode/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/commandcode/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/commandcode/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
