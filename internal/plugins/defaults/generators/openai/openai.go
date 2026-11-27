package openai

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/openai/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/openai/generate"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config, registries.Provider)
	generate.Register(registries.Generation, registries.Provider, registries.Credentials)
}
