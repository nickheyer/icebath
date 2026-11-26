package custom

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/custom/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/custom/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/custom/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
