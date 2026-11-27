package firecrawl

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/browsers/firecrawl/browser"
	"github.com/nickheyer/icebath/internal/plugins/defaults/browsers/firecrawl/config"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	browser.Register(registries.Browser)
}
