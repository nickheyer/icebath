package xiaomi

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xiaomi/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xiaomi/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/xiaomi/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
