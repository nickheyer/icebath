package nvidia

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nvidia/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nvidia/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/nvidia/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
