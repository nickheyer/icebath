package secretcommands

import (
	"github.com/nickheyer/icebath/internal/cli"
	secretscli "github.com/nickheyer/icebath/internal/secrets/cli"
)

func Register(host *secretscli.Commands) *cli.Registry {
	registry := new(cli.Registry)
	host.Register(registry)
	return registry
}
