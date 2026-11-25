package minimax

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/minimax/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/minimax/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/minimax/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/minimax/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
