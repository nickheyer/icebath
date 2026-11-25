package nvidia

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nvidia/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nvidia/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nvidia/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nvidia/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
