package computeruse

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/cua"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	packages.Register(registries.Packages)
	driver := cua.New(registries.Config, registries.Terminal.Host(), registries.Packages.Host(), registries.Desktop.Host())
	tools.Register(registries.Tools, registries.Sessions, driver, registries.Approvals.Host())
	server.Register(registries.Server, registries.Config, driver)
	cli.Register(registries.CLI)
}
