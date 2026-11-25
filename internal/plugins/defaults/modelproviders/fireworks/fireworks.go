package fireworks

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/fireworks/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/fireworks/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/fireworks/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/fireworks/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
