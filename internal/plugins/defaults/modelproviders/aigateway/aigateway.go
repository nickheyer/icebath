package aigateway

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/aigateway/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/aigateway/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/aigateway/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/aigateway/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
