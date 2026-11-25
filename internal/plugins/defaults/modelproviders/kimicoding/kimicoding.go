package kimicoding

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kimicoding/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kimicoding/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kimicoding/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/kimicoding/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
