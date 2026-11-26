package gmi

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gmi/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gmi/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/gmi/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
