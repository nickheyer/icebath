package deepinfra

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/deepinfra/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/deepinfra/generate"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	generate.Register(registries.Generation, registries.Provider, registries.Credentials, registries.Models.Host())
}
