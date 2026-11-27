package bitwarden

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/secrets"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/vault"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	packages.Register(registries.Packages)
	store := secrets.Register(registries.Secrets, registries.Packages)
	vault.Register(registries.Vault)
	server.Register(registries.Server, registries.Config, store)
	cli.Register(registries.SecretCommands)
	app.Register(registries.SecretStores)
}
