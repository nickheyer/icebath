package gemininative

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/transports/gemininative/transport"
)

func Register(registries *plugins.Registries) {
	transport.Register(registries.Transport, registries.Config, registries.Provider, registries.Models.Host())
}
