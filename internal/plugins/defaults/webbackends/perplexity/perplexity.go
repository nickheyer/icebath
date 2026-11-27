package perplexity

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/perplexity/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/perplexity/web"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	web.Register(registries.Web)
}
