package secrets

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/command"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword"
)

func Register(registries *plugins.Registries) {
	bitwarden.Register(registries)
	onepassword.Register(registries)
	command.Register(registries)
}
