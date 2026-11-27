package openai

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/openai/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/openai/service"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	service.Register(registries.Speech)
}
