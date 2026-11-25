package lmstudio

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/lmstudio/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/lmstudio/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/lmstudio/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/lmstudio/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
