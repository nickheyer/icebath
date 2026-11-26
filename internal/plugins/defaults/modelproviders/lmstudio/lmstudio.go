package lmstudio

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/lmstudio/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/lmstudio/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/lmstudio/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
