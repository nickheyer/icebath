package browserbase

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/browsers/browserbase/browser"
	"github.com/nickheyer/icebath/internal/plugins/defaults/browsers/browserbase/config"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	browser.Register(registries.Browser)
}
