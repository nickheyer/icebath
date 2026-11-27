package sherpa

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/sherpa/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/sherpa/service"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	service.Register(registries.Speech)
}
