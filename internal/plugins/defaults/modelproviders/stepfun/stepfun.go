package stepfun

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/stepfun/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/stepfun/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/stepfun/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
