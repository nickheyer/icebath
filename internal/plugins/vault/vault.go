package vault

import "github.com/nickheyer/icebath/internal/vault"

func Register(host *vault.Vault) *vault.Registry {
	registry := new(vault.Registry)
	host.Register(registry)
	return registry
}
