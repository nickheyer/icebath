package firecrawl

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/firecrawl/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/firecrawl/web"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	web.Register(registries.Web)
}
