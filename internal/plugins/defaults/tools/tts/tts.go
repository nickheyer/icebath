package tts

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/tts/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/tts/tools"
)

func Register(registries *plugins.Registries) {
	tools.Register(registries.Tools, registries.Speech.Host())
	app.Register(registries.Views)
}
