package openrouter

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
