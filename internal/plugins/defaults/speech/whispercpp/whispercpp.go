package whispercpp

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/whispercpp/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/whispercpp/service"
)

func Register(registries *plugins.Registries) {
	packages.Register(registries.Packages)
	service.Register(registries.Speech, registries.Packages.Host())
}
