package deepseek

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepseek/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepseek/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepseek/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
