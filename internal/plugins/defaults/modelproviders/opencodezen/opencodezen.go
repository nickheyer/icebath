package opencodezen

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/opencodezen/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/opencodezen/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/opencodezen/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/opencodezen/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
