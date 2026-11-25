package provider

import "github.com/nickheyer/icebath/internal/provider"

func Register(host *provider.Provider) *provider.Registry {
	registry := new(provider.Registry)
	host.Register(registry)
	return registry
}
