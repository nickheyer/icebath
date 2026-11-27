package tavily

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/tavily/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/tavily/web"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	web.Register(registries.Web)
}
