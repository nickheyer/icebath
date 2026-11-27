package parallel

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/parallel/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/parallel/web"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	web.Register(registries.Web)
}
