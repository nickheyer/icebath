package slash

import "github.com/nickheyer/icebath/internal/slash"

func Register(host *slash.Slash) *slash.Registry {
	registry := new(slash.Registry)
	host.Register(registry)
	return registry
}
