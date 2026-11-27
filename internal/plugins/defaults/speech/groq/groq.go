package groq

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/groq/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/groq/service"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	service.Register(registries.Speech)
}
