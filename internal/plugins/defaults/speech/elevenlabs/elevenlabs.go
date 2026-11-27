package elevenlabs

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/elevenlabs/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/elevenlabs/service"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	service.Register(registries.Speech)
}
