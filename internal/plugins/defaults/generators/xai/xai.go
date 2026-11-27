package xai

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/xai/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/xai/generate"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/xai/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	tools.Register(registries.Tools, generate.Register(registries.Generation, registries.Provider, registries.Credentials))
}
