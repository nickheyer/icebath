package xai

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/xai/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/xai/service"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	service.Register(registries.Speech)
}
