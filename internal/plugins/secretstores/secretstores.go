package secretstores

import secretsapp "github.com/nickheyer/icebath/internal/secrets/app"

func Register(host *secretsapp.Stores) *secretsapp.Registry {
	registry := new(secretsapp.Registry)
	host.Register(registry)
	return registry
}
