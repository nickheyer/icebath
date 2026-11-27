package xsearch

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/xsearch/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/xsearch/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	tools.Register(registries.Tools, registries.Provider, registries.Credentials)
}
