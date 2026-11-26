package alibaba

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibaba/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibaba/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibaba/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
