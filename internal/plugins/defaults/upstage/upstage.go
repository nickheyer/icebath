package upstage

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/upstage/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/upstage/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/upstage/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/upstage/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
