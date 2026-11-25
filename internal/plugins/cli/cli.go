package cli

import "github.com/nickheyer/icebath/internal/cli"

func Register(host *cli.CLI) *cli.Registry {
	registry := new(cli.Registry)
	host.Register(registry)
	return registry
}
