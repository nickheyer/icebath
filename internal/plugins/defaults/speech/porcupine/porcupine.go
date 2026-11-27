package porcupine

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/porcupine/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/porcupine/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/porcupine/service"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	packages.Register(registries.Packages)
	service.Register(registries.Speech)
}
