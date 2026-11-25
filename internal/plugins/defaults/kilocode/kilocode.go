package kilocode

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/kilocode/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/kilocode/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/kilocode/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/kilocode/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
