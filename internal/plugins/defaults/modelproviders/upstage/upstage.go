package upstage

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/upstage/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/upstage/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/upstage/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
