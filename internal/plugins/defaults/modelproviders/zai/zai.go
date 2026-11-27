package zai

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/zai/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/zai/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/zai/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
