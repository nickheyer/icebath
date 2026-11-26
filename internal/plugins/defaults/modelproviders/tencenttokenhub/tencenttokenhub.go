package tencenttokenhub

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/tencenttokenhub/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/tencenttokenhub/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/tencenttokenhub/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
