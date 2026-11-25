package alibabacodingplan

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/alibabacodingplan/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/alibabacodingplan/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/alibabacodingplan/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/alibabacodingplan/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
