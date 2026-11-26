package metaai

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/metaai/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/metaai/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/metaai/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
