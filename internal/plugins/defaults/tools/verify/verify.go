package verify

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/verify/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/verify/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/verify/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	// The person hears of a ledger file set aside where the session store's notices reach them
	ledger := tools.Register(registries.Tools, registries.Sessions.Tell)
	cli.Register(registries.CLI, registries.Config, ledger, registries.Terminal.Host())
}
