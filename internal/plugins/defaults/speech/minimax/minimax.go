package minimax

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/minimax/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/minimax/service"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	service.Register(registries.Speech)
}
