package custom

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/custom/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/custom/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/custom/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/custom/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models, registries.Config)
}
