package secrets

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/command"
	"github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword"
)

// In the order the stores and the password managers are listed and asked by default
func Register(registries *plugins.Registries) {
	onepassword.Register(registries)
	bitwarden.Register(registries)
	command.Register(registries)
}
