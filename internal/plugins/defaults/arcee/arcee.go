package arcee

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/arcee/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/arcee/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/arcee/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/arcee/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
