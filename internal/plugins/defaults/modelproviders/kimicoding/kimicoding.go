package kimicoding

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kimicoding/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kimicoding/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kimicoding/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
