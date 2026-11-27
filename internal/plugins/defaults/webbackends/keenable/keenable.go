package keenable

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/keenable/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/keenable/web"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	web.Register(registries.Web)
}
