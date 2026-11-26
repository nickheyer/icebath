package opencodezen

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/opencodezen/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/opencodezen/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/opencodezen/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
