package krea

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/krea/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/generators/krea/generate"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	generate.Register(registries.Generation)
}
