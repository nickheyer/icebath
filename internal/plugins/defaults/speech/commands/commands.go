package commands

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/commands/service"
)

func Register(registries *plugins.Registries) {
	service.Register(registries.Speech)
}
