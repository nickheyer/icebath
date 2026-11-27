package openwakeword

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/openwakeword/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/openwakeword/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/openwakeword/service"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	packages.Register(registries.Packages)
	service.Register(registries.Speech)
}
