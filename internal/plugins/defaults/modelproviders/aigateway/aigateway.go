package aigateway

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/aigateway/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/aigateway/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/aigateway/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
