// The browser tools as a plugin: Chromium declared to the package host, browser_exec, browser_cdp, browser_dialog and browser_vault, /browser
// and `icebath browser`
package browser

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/browser/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/browser/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/browser/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/browser/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/browser/tools"
)

func Register(registries *plugins.Registries) {
	packages.Register(registries.Packages)
	b := tools.Register(registries.Tools, registries.Sessions, registries.Profile, registries.Config, registries.Browser.Host(),
		registries.Desktop.Host(), registries.Terminal.Host(), registries.Vault.Host(), registries.Packages.Host())
	server.Register(registries.Server, registries.Config, b)
	app.Register(registries.App)
	cli.Register(registries.CLI, tools.Browsers())
}
