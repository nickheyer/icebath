package metaai

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/metaai/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/metaai/generate"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	generate.Register(registries.Generation, registries.Provider, registries.Credentials)
}
