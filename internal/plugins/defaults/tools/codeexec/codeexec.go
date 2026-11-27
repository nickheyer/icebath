package codeexec

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/codeexec/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/codeexec/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	tools.Register(registries.Tools, registries.Sessions, registries.Config, registries.Terminal.Host(), registries.Packages.Host(),
		registries.Approvals.Host())
}
