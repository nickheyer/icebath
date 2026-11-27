package ddgs

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/ddgs/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/ddgs/web"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	web.Register(registries.Web)
}
