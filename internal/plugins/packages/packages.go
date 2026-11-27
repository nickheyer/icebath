package packages

import "github.com/nickheyer/icebath/internal/packages"

func Register(host *packages.Packages) *packages.Registry {
	registry := new(packages.Registry)
	host.Register(registry)
	return registry
}
