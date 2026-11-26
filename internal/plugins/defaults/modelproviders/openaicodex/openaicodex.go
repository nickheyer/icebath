package openaicodex

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	models.Register(registries.Models)
}
