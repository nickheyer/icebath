package auxiliary

import "github.com/nickheyer/icebath/internal/auxiliary"

func Register(host *auxiliary.Auxiliary) *auxiliary.Registry {
	registry := new(auxiliary.Registry)
	host.Register(registry)
	return registry
}
