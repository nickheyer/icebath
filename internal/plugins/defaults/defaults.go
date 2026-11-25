package defaults

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/agents"
	"github.com/nickheyer/icebath/internal/plugins/defaults/examples"
	"github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders"
	"github.com/nickheyer/icebath/internal/plugins/defaults/sessions"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools"
	"github.com/nickheyer/icebath/internal/plugins/defaults/transports"
)

func Register(registries *plugins.Registries) {
	examples.Register(registries)
	sessions.Register(registries)
	transports.Register(registries)
	modelproviders.Register(registries)
	tools.Register(registries)
	agents.Register(registries)
}
