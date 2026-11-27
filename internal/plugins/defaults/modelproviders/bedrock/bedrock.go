package bedrock

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/bedrock/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/bedrock/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/bedrock/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
