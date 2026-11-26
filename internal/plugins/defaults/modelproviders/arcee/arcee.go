package arcee

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/arcee/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/arcee/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/arcee/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
