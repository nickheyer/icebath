package gmi

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/gmi/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/gmi/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/gmi/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/gmi/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
