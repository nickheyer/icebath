package terminal

import "github.com/nickheyer/icebath/internal/terminal"

func Register(host *terminal.Terminal) *terminal.Registry {
	registry := new(terminal.Registry)
	host.Register(registry)
	return registry
}
