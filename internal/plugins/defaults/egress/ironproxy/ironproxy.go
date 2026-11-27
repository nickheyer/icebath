package ironproxy

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/slash"
	"github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/terminal"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	packages.Register(registries.Packages)
	egress := terminal.Register(registries.Terminal, registries.Profile, registries.Config, registries.Packages, registries.Secrets)
	server.Register(registries.Server, registries.Config, egress, registries.Secrets)
	slash.Register(registries.Slash, registries.Config, egress, registries.Sessions)
	cli.Register(registries.CLI)
}
