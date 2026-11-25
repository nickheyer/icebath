package xai

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/xai/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/xai/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/xai/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/xai/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
