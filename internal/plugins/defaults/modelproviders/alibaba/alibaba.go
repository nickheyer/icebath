package alibaba

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibaba/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibaba/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibaba/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibaba/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
