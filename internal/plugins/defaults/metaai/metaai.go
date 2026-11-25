package metaai

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/metaai/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/metaai/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/metaai/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/metaai/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
