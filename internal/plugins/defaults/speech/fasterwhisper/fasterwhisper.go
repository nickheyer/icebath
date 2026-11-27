package fasterwhisper

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/fasterwhisper/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/fasterwhisper/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/fasterwhisper/service"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	packages.Register(registries.Packages)
	service.Register(registries.Speech)
}
