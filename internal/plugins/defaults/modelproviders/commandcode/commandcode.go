package commandcode

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/commandcode/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/commandcode/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/commandcode/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
