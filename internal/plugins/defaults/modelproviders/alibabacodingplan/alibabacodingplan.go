package alibabacodingplan

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibabacodingplan/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibabacodingplan/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/alibabacodingplan/provider"
)

func Register(registries *plugins.Registries) {
	provider.Register(registries.Provider)
	credentials.Register(registries.Credentials)
	models.Register(registries.Models)
}
