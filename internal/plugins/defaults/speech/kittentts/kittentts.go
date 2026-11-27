package kittentts

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/kittentts/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/kittentts/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/kittentts/service"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	packages.Register(registries.Packages)
	service.Register(registries.Speech)
}
