package hooks

import "github.com/nickheyer/icebath/internal/hooks"

func Register(host *hooks.Hooks) *hooks.Registry {
	registry := new(hooks.Registry)
	host.Register(registry)
	return registry
}
