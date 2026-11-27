package generation

import "github.com/nickheyer/icebath/internal/generation"

func Register(host *generation.Generation) *generation.Registry {
	registry := new(generation.Registry)
	host.Register(registry)
	return registry
}
