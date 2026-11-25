package transport

import "github.com/nickheyer/icebath/internal/transport"

func Register(host *transport.Transport) *transport.Registry {
	registry := new(transport.Registry)
	host.Register(registry)
	return registry
}
