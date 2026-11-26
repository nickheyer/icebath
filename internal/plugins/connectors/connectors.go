package connectors

import providerapp "github.com/nickheyer/icebath/internal/provider/app"

func Register(host *providerapp.Connectors) *providerapp.Registry {
	registry := new(providerapp.Registry)
	host.Register(registry)
	return registry
}
