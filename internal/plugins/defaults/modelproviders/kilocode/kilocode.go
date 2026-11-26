package kilocode

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kilocode/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kilocode/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kilocode/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
