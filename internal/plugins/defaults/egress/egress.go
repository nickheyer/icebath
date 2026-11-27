package egress

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy"
)

func Register(registries *plugins.Registries) {
	ironproxy.Register(registries)
}
