package ollamacloud

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/ollamacloud/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/ollamacloud/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/ollamacloud/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/ollamacloud/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
