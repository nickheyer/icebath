package codexresponses

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/transports/codexresponses/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/transports/codexresponses/transport"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	transport.Register(registries.Transport, registries.Config, registries.Provider, registries.Models.Host())
}
