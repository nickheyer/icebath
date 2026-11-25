package deepseek

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/deepseek/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/deepseek/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/deepseek/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/deepseek/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
