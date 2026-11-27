package verify

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/verify/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/verify/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/verify/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	ledger := tools.Register(registries.Tools)
	cli.Register(registries.CLI, registries.Config, ledger, registries.Terminal.Host())
}
