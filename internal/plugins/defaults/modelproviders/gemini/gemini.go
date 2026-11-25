package gemini

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gemini/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gemini/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gemini/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gemini/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
