package deepinfra

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/deepinfra/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/deepinfra/service"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	service.Register(registries.Speech, registries.Models.Host())
}
