package contexts

import "github.com/nickheyer/icebath/internal/contexts"

func Register(host *contexts.Contexts) *contexts.Registry {
	registry := new(contexts.Registry)
	host.Register(registry)
	return registry
}
