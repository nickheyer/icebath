package actual

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/actual/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/actual/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/actual/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
