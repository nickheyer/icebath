package fal

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/fal/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/fal/generate"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	generate.Register(registries.Generation)
}
