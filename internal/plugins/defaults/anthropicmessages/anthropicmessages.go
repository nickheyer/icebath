package anthropicmessages

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/anthropicmessages/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/anthropicmessages/transport"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	transport.Register(registries.Transport, registries.Config)
}
