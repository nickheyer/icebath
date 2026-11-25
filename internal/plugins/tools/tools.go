package tools

import "github.com/nickheyer/icebath/internal/tools"

func Register(host *tools.Tools) *tools.Registry {
	registry := new(tools.Registry)
	host.Register(registry)
	return registry
}
