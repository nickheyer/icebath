package chatcompletions

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/transports/chatcompletions/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/transports/chatcompletions/transport"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	transport.Register(registries.Transport, registries.Config, registries.Provider, registries.Models.Host())
}
