package deepinfra

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/deepinfra/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/deepinfra/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/deepinfra/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/deepinfra/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
