package bedrock

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/bedrock/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/bedrock/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/bedrock/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
}
