package githubcopilot

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/githubcopilot/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/githubcopilot/credentials"
	"github.com/nickheyer/icebath/internal/plugins/defaults/githubcopilot/models"
	"github.com/nickheyer/icebath/internal/plugins/defaults/githubcopilot/provider"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	provider.Register(registries.Provider, registries.Config)
	credentials.Register(registries.Credentials, registries.Config)
	models.Register(registries.Models)
}
