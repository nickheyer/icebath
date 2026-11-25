package chatcompletions

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/transports/chatcompletions/transport"
)

func Register(registries *plugins.Registries) {
	transport.Register(registries.Transport)
}
