package alibaba

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/alibaba/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/alibaba/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/alibaba/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/alibaba/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
