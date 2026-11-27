package edge

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/edge/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/edge/service"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	service.Register(registries.Speech)
}
