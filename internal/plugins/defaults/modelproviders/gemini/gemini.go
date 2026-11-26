package gemini

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gemini/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gemini/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gemini/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
