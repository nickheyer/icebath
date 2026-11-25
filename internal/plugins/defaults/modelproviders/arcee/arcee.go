package arcee

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/arcee/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/arcee/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/arcee/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/arcee/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
