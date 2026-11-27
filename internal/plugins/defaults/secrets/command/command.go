package command

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/command/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/command/secrets"
)

func Register(registries *plugins.Registries) {
	config.Register(registries.Config)
	secrets.Register(registries.Secrets)
}
