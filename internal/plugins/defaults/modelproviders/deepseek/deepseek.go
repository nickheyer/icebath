package deepseek

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepseek/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepseek/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepseek/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepseek/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
