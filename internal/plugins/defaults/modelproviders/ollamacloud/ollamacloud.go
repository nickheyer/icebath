package ollamacloud

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/ollamacloud/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/ollamacloud/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/ollamacloud/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
