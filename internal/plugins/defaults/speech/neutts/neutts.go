package neutts

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/neutts/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/neutts/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/neutts/service"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	packages.Register(registries.Packages)
	service.Register(registries.Speech)
}
