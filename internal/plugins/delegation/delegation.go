package delegation

import "github.com/nickheyer/icebath/internal/delegation"

func Register(host *delegation.Delegation) *delegation.Registry {
	registry := new(delegation.Registry)
	host.Register(registry)
	return registry
}
