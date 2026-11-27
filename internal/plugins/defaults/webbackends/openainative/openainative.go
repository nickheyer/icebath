package openainative

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/openainative/web"
)

func Register(registries *plugins.Registries) {
	web.Register(registries.Web, registries.Provider, registries.Credentials)
}
