package launch

import "github.com/nickheyer/icebath/internal/launch"

func Register(host *launch.Launch) *launch.Registry {
	registry := new(launch.Registry)
	host.Register(registry)
	return registry
}
