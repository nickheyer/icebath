package onepassword

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/app"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/cli"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/secrets"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/server"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/vault"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	store := secrets.Register(registries.Secrets)
	vault.Register(registries.Vault)
	server.Register(registries.Server, registries.Config, store)
	cli.Register(registries.SecretCommands)
	app.Register(registries.SecretStores)
}
