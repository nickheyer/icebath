package anthropicmessages

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/transports/anthropicmessages/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/transports/anthropicmessages/transport"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	transport.Register(registries.Transport, registries.Config, registries.Provider, registries.Models.Host())
}
