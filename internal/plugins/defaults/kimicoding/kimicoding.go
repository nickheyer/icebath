package kimicoding

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/kimicoding/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/kimicoding/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/kimicoding/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/kimicoding/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
