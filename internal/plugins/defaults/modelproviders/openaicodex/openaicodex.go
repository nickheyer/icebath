package openaicodex

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	models.Register(registries.Models)
}
