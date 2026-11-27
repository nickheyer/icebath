// Camofox as a browser plugin: the camofox browser tools on a self hosted camofox-browser server, its tab a page browser_vault fills, and
// `icebath camofox`
package camofox

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/camofox/auxiliary"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/camofox/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/camofox/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/camofox/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/camofox/tools"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	auxiliary.Register(registries.Auxiliary)
	c := tools.Register(registries.Tools, registries.Sessions, registries.Browser, registries.Config)
	server.Register(registries.Server, registries.Config, c)
	cli.Register(registries.CLI)
}
