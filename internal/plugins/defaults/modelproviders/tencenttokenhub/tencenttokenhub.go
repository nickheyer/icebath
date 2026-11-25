package tencenttokenhub

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/tencenttokenhub/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/tencenttokenhub/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/tencenttokenhub/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/tencenttokenhub/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
