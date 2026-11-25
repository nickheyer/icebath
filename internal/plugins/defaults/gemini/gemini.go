package gemini

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/gemini/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/gemini/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/gemini/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/gemini/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
