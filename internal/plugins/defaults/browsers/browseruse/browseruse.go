package browseruse

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/browsers/browseruse/browser"
	"github.com/nickheyer/icebath/internal/plugins/defaults/browsers/browseruse/config"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	browser.Register(registries.Browser)
}
