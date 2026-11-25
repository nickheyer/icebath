package novita

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/novita/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/novita/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/novita/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/novita/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
