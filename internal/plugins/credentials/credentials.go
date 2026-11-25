package credentials

import "github.com/nickheyer/icebath/internal/credentials"

func Register(host *credentials.Credentials) *credentials.Registry {
	registry := new(credentials.Registry)
	host.Register(registry)
	return registry
}
