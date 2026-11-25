package xiaomi

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/xiaomi/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/xiaomi/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/xiaomi/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/xiaomi/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
