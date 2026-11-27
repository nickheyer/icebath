package xai

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/xai/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/xai/web"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	web.Register(registries.Web, registries.Provider, registries.Credentials)
}
