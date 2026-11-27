package secrets

import "github.com/nickheyer/icebath/internal/secrets"

func Register(host *secrets.Secrets) *secrets.Registry {
	registry := new(secrets.Registry)
	host.Register(registry)
	return registry
}
