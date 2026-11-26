package openaiapi

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaiapi/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaiapi/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaiapi/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
