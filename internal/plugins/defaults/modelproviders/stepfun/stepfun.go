package stepfun

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/stepfun/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/stepfun/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/stepfun/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/stepfun/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
