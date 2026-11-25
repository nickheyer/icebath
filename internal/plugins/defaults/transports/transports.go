package transports

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/transports/anthropicmessages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/transports/chatcompletions"
	"github.com/nickheyer/icebath/internal/plugins/defaults/transports/codexresponses"
)

func Register(registries *plugins.Registries) {
	anthropicmessages.Register(registries)
	chatcompletions.Register(registries)
	codexresponses.Register(registries)
}
