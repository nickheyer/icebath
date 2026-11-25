package openaiapi

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaiapi/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaiapi/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaiapi/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaiapi/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
