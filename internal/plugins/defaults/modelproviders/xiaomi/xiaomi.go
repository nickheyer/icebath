package xiaomi

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xiaomi/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xiaomi/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xiaomi/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xiaomi/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
